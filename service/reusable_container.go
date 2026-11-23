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
)

// ReusableContainer owns one WebDriver container for the lifetime of the server.
// Admission and release of sessions are serialized by Selenoid's single slot.
type ReusableContainer struct {
	docker         *Docker
	container      *StartedService
	version        string
	defaultVersion string
	saveAllLogs    bool
	androidChrome  bool
	ctx            context.Context
	fail           context.CancelCauseFunc
}

func StartReusableContainer(ctx context.Context, fail context.CancelCauseFunc, manager *DefaultManager, caps session.Caps) (*ReusableContainer, error) {
	caps.ScreenResolution = "1920x1080x24"
	starter, ok := manager.Find(caps, 0)
	docker, isDocker := starter.(*Docker)
	if !ok || !isDocker {
		return nil, errors.New("no Docker image for the configured browser")
	}
	server := &ReusableContainer{
		docker:         docker,
		version:        caps.Version,
		defaultVersion: manager.Config.Browsers[caps.Name].Default,
		ctx:            ctx,
		fail:           fail,
		saveAllLogs:    docker.SaveAllLogs,
		androidChrome:  caps.Name == "chrome" && strings.EqualFold(docker.Service.PlatformName, "android"),
	}
	// Capture logs on session release, independently of container shutdown.
	docker.SaveAllLogs = false
	startCtx, cancel := context.WithTimeout(ctx, docker.StartupTimeout)
	defer cancel()
	container, err := docker.start(startCtx, func(u *url.URL) error {
		return waitWebDriver(startCtx, u, server.androidChrome)
	})
	if err != nil {
		return nil, err
	}
	server.container = container
	if server.androidChrome {
		if err := server.resetChrome(startCtx); err != nil {
			server.Close()
			return nil, err
		}
	}
	return server, nil
}

func (a *ReusableContainer) PrepareNextSession(ctx context.Context) error {
	if a.androidChrome {
		if err := a.resetChrome(ctx); err != nil {
			return err
		}
	}
	endpoint := *a.container.Url
	return waitWebDriver(ctx, &endpoint, a.androidChrome)
}

func waitWebDriver(ctx context.Context, endpoint *url.URL, androidChrome bool) error {
	base := *endpoint
	status := base.JoinPath("status").String()
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
			if resp.StatusCode == http.StatusNotFound && base.Path == "" {
				base.Path = "/wd/hub"
				status = base.JoinPath("status").String()
				continue
			}
			// Appium 1.x reports its build version without a W3C ready field.
			ready := result.Value.Ready
			if err == nil && resp.StatusCode == http.StatusOK && ((ready != nil && *ready) ||
				(ready == nil && androidChrome && strings.HasPrefix(result.Value.Build.Version, "1."))) {
				if endpoint.Path != base.Path {
					endpoint.Path = base.Path
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for WebDriver %s: %w", status, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (a *ReusableContainer) Close() { a.container.Cancel() }

func (a *ReusableContainer) Fail(err error) { a.fail(err) }

type reusedSessionStart func() (*StartedService, error)

func (start reusedSessionStart) StartWithCancel() (*StartedService, error) { return start() }

func (a *ReusableContainer) Find(caps session.Caps, requestID uint64) (Starter, bool) {
	version := caps.Version
	if version == "" {
		version = a.defaultVersion
	}
	if caps.BrowserName() != a.docker.Name || version == "" || !strings.HasPrefix(a.version, version) ||
		(a.docker.Service.PlatformName != "" && caps.Platform != "" && !strings.EqualFold(caps.Platform, a.docker.Service.PlatformName)) {
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
	return reusedSessionStart(func() (*StartedService, error) {
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
