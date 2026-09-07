package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func runCommand(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("run command: empty binary name")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if ctx.Err() != nil {
		runErr = fmt.Errorf("%s canceled: %w", name, ctx.Err())
	} else if runErr != nil {
		runErr = fmt.Errorf("%s failed: %w", name, runErr)
	}
	if runErr == nil {
		return outBuf.Bytes(), nil
	}
	var parts []string
	if errBuf.Len() > 0 {
		parts = append(parts, "stderr:\n"+strings.TrimSpace(errBuf.String()))
	}
	if outBuf.Len() > 0 {
		parts = append(parts, "stdout:\n"+strings.TrimSpace(outBuf.String()))
	}
	if len(parts) > 0 {
		runErr = fmt.Errorf("%w\n%s", runErr, strings.Join(parts, "\n\n"))
	}
	return outBuf.Bytes(), runErr
}
