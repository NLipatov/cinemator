//go:build unix && !linux

package cli

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func configureOwnedProcess(ctx context.Context, cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = append(cmd.ExtraFiles, processGuards(ctx)...)
}

func signalOwnedProcess(cmd *exec.Cmd, force bool) error {
	if cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
