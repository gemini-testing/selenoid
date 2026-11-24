package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aerokube/selenoid/session"
	"github.com/docker/docker/errdefs"
)

// ContainerPool runs one configured WebDriver image in a fixed number of slots.
type ContainerPool struct {
	docker         *Docker
	defaultVersion string
	saveAllLogs    bool
	androidChrome  bool
	ctx            context.Context
	fail           context.CancelCauseFunc
	slots          []*StartedService
	idle           chan int
	// Each checked-out slot has one owner. Close waits for any replacements.
	lifecycle sync.RWMutex
}

func StartContainerPool(ctx context.Context, fail context.CancelCauseFunc, manager *DefaultManager, caps session.Caps, size int) (*ContainerPool, error) {
	caps.ScreenResolution = "1920x1080x24"
	starter, ok := manager.Find(caps, 0)
	docker, isDocker := starter.(*Docker)
	if !ok || !isDocker || size < 1 {
		return nil, errors.New("expected a Docker browser and a positive pool size")
	}
	p := &ContainerPool{
		docker: docker, defaultVersion: manager.Config.Browsers[caps.Name].Default,
		saveAllLogs:   docker.SaveAllLogs,
		androidChrome: caps.Name == "chrome" && strings.EqualFold(docker.Service.PlatformName, "android"),
		ctx:           ctx, fail: fail, slots: make([]*StartedService, size), idle: make(chan int, size),
	}
	docker.SaveAllLogs = false // Capture logs per session, before returning its slot.
	startErrs := make([]error, size)
	var starts sync.WaitGroup
	for i := range p.slots {
		starts.Go(func() {
			p.slots[i], startErrs[i] = p.start()
			if startErrs[i] == nil {
				p.idle <- i
			}
		})
	}
	starts.Wait()
	if err := errors.Join(append(startErrs, context.Cause(ctx))...); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return p, nil
}

func (p *ContainerPool) start() (*StartedService, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.docker.StartupTimeout)
	defer cancel()
	container, err := p.docker.start(ctx, func(u *url.URL) error { return waitWebDriver(ctx, u, p.androidChrome) }, p.remove)
	if err != nil {
		return nil, err
	}
	if p.androidChrome {
		if err := p.docker.resetChrome(ctx, container.Container.ID); err != nil {
			return nil, errors.Join(err, p.remove(container.Container.ID))
		}
	}
	return container, nil
}

func (p *ContainerPool) Close() error {
	p.fail(nil)
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	errs := make([]error, len(p.slots))
	var removals sync.WaitGroup
	for i, container := range p.slots {
		if container != nil {
			removals.Go(func() { errs[i] = p.remove(container.Container.ID) })
		}
	}
	removals.Wait()
	return errors.Join(errs...)
}

func (p *ContainerPool) remove(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.docker.SessionDeleteTimeout)
	defer cancel()
	for {
		err := removeContainer(ctx, p.docker.Client, p.docker.RequestId, id, p.docker.Environment)
		if !errdefs.IsConflict(err) {
			if err != nil {
				return fmt.Errorf("remove container %s: %w", id, err)
			}
			return nil
		}
		// Docker may already be removing an AutoRemove container.
		select {
		case <-ctx.Done():
			return fmt.Errorf("remove container %s: %w", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (p *ContainerPool) prepare(container *StartedService) error {
	ctx, cancel := context.WithTimeout(p.ctx, p.docker.SessionDeleteTimeout)
	defer cancel()
	if p.androidChrome {
		if err := p.docker.resetChrome(ctx, container.Container.ID); err != nil {
			return err
		}
	}
	endpoint := *container.Url
	return waitWebDriver(ctx, &endpoint, p.androidChrome)
}

func (p *ContainerPool) release(slot int, sessionErr error) error {
	p.lifecycle.RLock()
	defer p.lifecycle.RUnlock()
	if err := context.Cause(p.ctx); err != nil {
		return errors.Join(sessionErr, err)
	}
	container := p.slots[slot]
	if sessionErr == nil {
		sessionErr = p.prepare(container)
	}
	if sessionErr != nil {
		log.Printf("[-] [REPLACING_CONTAINER] [%s] [%v]", container.Container.ID, sessionErr)
		if err := p.replace(slot); err != nil {
			err = fmt.Errorf("replace container %s: %w", container.Container.ID, err)
			p.fail(err)
			return errors.Join(sessionErr, err)
		}
	}
	p.idle <- slot
	return sessionErr
}

func (p *ContainerPool) replace(slot int) error {
	previous := p.slots[slot]
	if err := p.remove(previous.Container.ID); err != nil {
		return err
	}
	replacement, err := p.start()
	p.slots[slot] = replacement
	return err
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

type reusedSessionStart func() (*StartedService, error)

func (start reusedSessionStart) StartWithCancel() (*StartedService, error) { return start() }

func (p *ContainerPool) Find(caps session.Caps, requestID uint64) (Starter, bool) {
	version := caps.Version
	if version == "" {
		version = p.defaultVersion
	}
	if caps.BrowserName() != p.docker.Name || version == "" || !strings.HasPrefix(p.docker.Version, version) ||
		(p.docker.Service.PlatformName != "" && caps.Platform != "" && !strings.EqualFold(caps.Platform, p.docker.Service.PlatformName)) {
		return nil, false
	}
	if caps.ScreenResolution != p.docker.ScreenResolution || caps.Video || caps.VNC ||
		caps.Skin != "" || caps.TimeZone != "" || caps.ContainerHostname != "" || caps.VideoCodec != "" ||
		len(caps.ApplicationContainers) > 0 || len(caps.AdditionalNetworks) > 0 || len(caps.HostsEntries) > 0 ||
		len(caps.DNSServers) > 0 || len(caps.Labels) > 0 {
		return nil, false
	}
	for _, env := range caps.Env {
		if !slices.Contains(p.docker.Service.Env, env) {
			return nil, false
		}
	}
	return reusedSessionStart(func() (*StartedService, error) {
		var slot int
		select {
		case <-p.ctx.Done():
			return nil, context.Cause(p.ctx)
		case slot = <-p.idle:
		}
		if err := context.Cause(p.ctx); err != nil {
			return nil, err
		}
		s := *p.slots[slot]
		since := time.Now().UTC().Format(time.RFC3339Nano)
		s.Cancel = func() {
			if (p.saveAllLogs || caps.Log) && p.docker.LogOutputDir != "" {
				ctx, cancel := context.WithTimeout(context.Background(), p.docker.SessionDeleteTimeout)
				defer cancel()
				p.docker.copyLogs(ctx, requestID, s.Container.ID, caps.LogName, since)
			}
		}
		s.ReleaseContainer = func(err error) error { return p.release(slot, err) }
		return &s, nil
	}), true
}
