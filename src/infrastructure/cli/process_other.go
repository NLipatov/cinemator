//go:build !unix && !windows

package cli

import (
	"context"
	"errors"
	"os/exec"
)

func configureOwnedProcess(context.Context, *exec.Cmd) {}

type ownedProcess struct{}

func attachOwnedProcess(*exec.Cmd) (*ownedProcess, error) {
	return nil, errors.New("owned process trees are unsupported on this platform")
}

func (*ownedProcess) signal(bool) error { return nil }

func (*ownedProcess) close() error { return nil }
