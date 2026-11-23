package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aerokube/selenoid/session"
	"github.com/docker/docker/api/types"
)

// WarmAndroid owns one Android container for the lifetime of the server.
// Admission and release of sessions are serialized by Selenoid's single slot.
type WarmAndroid struct {
	docker         *Docker
	container      *StartedService
	version        string
	defaultVersion string
	saveAllLogs    bool
	ctx            context.Context
	fail           context.CancelCauseFunc
}

func StartWarmAndroid(ctx context.Context, fail context.CancelCauseFunc, manager *DefaultManager) (*WarmAndroid, error) {
	browsers := manager.Config.Browsers
	chrome := browsers["chrome"]
	if len(browsers) != 1 || len(chrome.Versions) != 1 || manager.Client == nil {
		return nil, errors.New("warm Android requires Docker and exactly one chrome version in browsers.json")
	}
	var version string
	for version = range chrome.Versions {
	}
	caps := session.Caps{Name: "chrome", Version: version, ScreenResolution: "1920x1080x24"}
	starter, ok := manager.Find(caps, 0)
	docker, isDocker := starter.(*Docker)
	if !ok || !isDocker || version == "" {
		return nil, errors.New("warm Android requires a Chrome Docker image with a named version")
	}
	// Session logs are captured for each lease, independently of container cleanup.
	saveAllLogs := docker.SaveAllLogs
	docker.SaveAllLogs = false
	startCtx, cancel := context.WithTimeout(ctx, docker.StartupTimeout)
	defer cancel()
	container, err := docker.start(startCtx, func(u *url.URL) error { return waitAppium(startCtx, u) })
	if err != nil {
		return nil, err
	}
	android := &WarmAndroid{docker: docker, container: container, version: version, defaultVersion: chrome.Default, ctx: ctx, fail: fail, saveAllLogs: saveAllLogs}
	if err := android.ResetChrome(startCtx); err != nil {
		android.Close()
		return nil, err
	}
	return android, nil
}

func waitAppium(ctx context.Context, endpoint *url.URL) error {
	status := endpoint.JoinPath("status").String()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, status, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var result struct {
				Value struct {
					Ready *bool `json:"ready"`
					Build struct {
						Version string `json:"version"`
					} `json:"build"`
				} `json:"value"`
			}
			err = json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			// Appium 1.x reports its build version without a W3C ready field.
			ready := result.Value.Ready
			if err == nil && resp.StatusCode == http.StatusOK && ((ready != nil && *ready) ||
				(ready == nil && strings.HasPrefix(result.Value.Build.Version, "1."))) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for Appium %s: %w", status, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (a *WarmAndroid) Close() { a.container.Cancel() }

func (a *WarmAndroid) Fail(err error) { a.fail(err) }

// ResetChrome requires a booted device and clears Chrome's data and running processes.
func (a *WarmAndroid) ResetChrome(ctx context.Context) error {
	cmd := []string{"sh", "-ec", `test "$(adb shell getprop sys.boot_completed | tr -d '\r')" = 1 && test "$(adb shell pm clear com.android.chrome | tr -d '\r')" = Success`}
	exec, err := a.docker.Client.ContainerExecCreate(ctx, a.container.Container.ID, types.ExecConfig{Cmd: cmd})
	if err != nil {
		return fmt.Errorf("create Chrome reset: %w", err)
	}
	if err := a.docker.Client.ContainerExecStart(ctx, exec.ID, types.ExecStartCheck{Detach: true}); err != nil {
		return fmt.Errorf("start Chrome reset: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := a.docker.Client.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return fmt.Errorf("inspect Chrome reset: %w", err)
		}
		if !state.Running {
			if state.ExitCode != 0 {
				return fmt.Errorf("Chrome reset failed (exit %d)", state.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type warmSessionStart func() (*StartedService, error)

func (start warmSessionStart) StartWithCancel() (*StartedService, error) { return start() }

func (a *WarmAndroid) Find(caps session.Caps, requestID uint64) (Starter, bool) {
	version := caps.Version
	if version == "" {
		version = a.defaultVersion
	}
	if caps.Name != "chrome" || version == "" || !strings.HasPrefix(a.version, version) ||
		(caps.Platform != "" && !strings.EqualFold(caps.Platform, "android")) {
		return nil, false
	}
	if caps.ScreenResolution != a.docker.ScreenResolution || caps.Video || caps.VNC ||
		caps.Skin != "" || caps.TimeZone != "" || caps.ContainerHostname != "" || caps.VideoCodec != "" ||
		len(caps.ApplicationContainers) > 0 || len(caps.AdditionalNetworks) > 0 || len(caps.HostsEntries) > 0 ||
		len(caps.DNSServers) > 0 || len(caps.Labels) > 0 {
		return nil, false
	}
	for _, env := range caps.Env {
		if !slices.Contains(a.docker.Service.Env, env) {
			return nil, false
		}
	}
	return warmSessionStart(func() (*StartedService, error) {
		if err := context.Cause(a.ctx); err != nil {
			return nil, err
		}
		s := *a.container
		since := time.Now().UTC().Format(time.RFC3339Nano)
		s.Cancel = func() {
			if (a.saveAllLogs || caps.Log) && a.docker.LogOutputDir != "" {
				ctx, cancel := context.WithTimeout(context.Background(), a.docker.SessionDeleteTimeout)
				defer cancel()
				a.docker.copyLogs(ctx, requestID, s.Container.ID, caps.LogName, since)
			}
		}
		return &s, nil
	}), true
}
