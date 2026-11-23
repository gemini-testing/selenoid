package service

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
)

// resetChrome requires a booted device and clears Chrome's data and running processes.
func (a *ReusableContainer) resetChrome(ctx context.Context) error {
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
