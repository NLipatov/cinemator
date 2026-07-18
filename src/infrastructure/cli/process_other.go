//go:build !unix

package cli

import (
	"context"
	"os/exec"
)

func configureOwnedProcess(context.Context, *exec.Cmd) {}

func signalOwnedProcess(cmd *exec.Cmd, _ bool) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
