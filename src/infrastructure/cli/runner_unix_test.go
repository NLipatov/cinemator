//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunWithStdinCancelsDescendantProcessGroup(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := RunWithStdin(ctx, nil, "sh", "-c", "sleep 30 & echo $! > \"$1\"; wait", "sh", pidPath)
		done <- err
	}()

	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child process did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RunWithStdin() error = %v", err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived cancellation: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunWithStdinFencesDescendantsAfterLeaderExit(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	_, err := RunWithStdin(
		context.Background(),
		nil,
		"sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $! > \"$1\"", "sh", pidPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d survived leader exit: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunWithStdinPassesProcessGuards(t *testing.T) {
	guard, err := os.Create(filepath.Join(t.TempDir(), "guard"))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	ctx := WithProcessGuards(context.Background(), guard)
	if _, err := RunWithStdin(ctx, nil, "sh", "-c", ": <&3"); err != nil {
		t.Fatalf("guard descriptor was not inherited: %v", err)
	}
}
