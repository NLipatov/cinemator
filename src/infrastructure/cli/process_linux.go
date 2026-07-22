//go:build linux

package cli

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

type ownedProcess struct {
	cmd *exec.Cmd
}

func configureOwnedProcess(ctx context.Context, cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.ExtraFiles = append(cmd.ExtraFiles, processGuards(ctx)...)
}

func attachOwnedProcess(cmd *exec.Cmd) (*ownedProcess, error) {
	return &ownedProcess{cmd: cmd}, nil
}

func (p *ownedProcess) signal(force bool) error {
	if p.cmd.Process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-p.cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (*ownedProcess) close() error { return nil }
