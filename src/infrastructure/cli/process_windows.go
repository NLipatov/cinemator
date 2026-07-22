//go:build windows

package cli

import (
	"context"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type ownedProcess struct {
	job windows.Handle
}

func configureOwnedProcess(context.Context, *exec.Cmd) {}

func attachOwnedProcess(cmd *exec.Cmd) (*ownedProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	closeJob := true
	defer func() {
		if closeJob {
			_ = windows.CloseHandle(job)
		}
	}()

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return nil, err
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return nil, err
	}
	closeJob = false
	return &ownedProcess{job: job}, nil
}

func (p *ownedProcess) signal(bool) error {
	return windows.TerminateJobObject(p.job, 1)
}

func (p *ownedProcess) close() error {
	return windows.CloseHandle(p.job)
}
