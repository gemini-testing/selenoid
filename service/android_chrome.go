package service

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
)

// resetChrome requires a booted device and clears Chrome's data and running processes.
func (d *Docker) resetChrome(ctx context.Context, containerID string) error {
	cmd := []string{"sh", "-ec", `until test "$(adb shell getprop sys.boot_completed | tr -d '\r')" = 1; do sleep 1; done; test "$(adb shell pm clear com.android.chrome | tr -d '\r')" = Success`}
	exec, err := d.Client.ContainerExecCreate(ctx, containerID, types.ExecConfig{Cmd: cmd})
	if err != nil {
		return fmt.Errorf("create Chrome reset: %w", err)
	}
	if err := d.Client.ContainerExecStart(ctx, exec.ID, types.ExecStartCheck{Detach: true}); err != nil {
		return fmt.Errorf("start Chrome reset: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := d.Client.ContainerExecInspect(ctx, exec.ID)
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
