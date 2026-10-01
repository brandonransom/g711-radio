//go:build windows

package main

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	childJobOnce sync.Once
	childJob     windows.Handle
	childJobErr  error
)

func prepareChildProcess(cmd *exec.Cmd) {}

// bindChildProcess adds the process to a job object that kills its members
// when this process exits for any reason, so a crash or forced stop of
// g711-radio can't leave whisper-server instances holding their ports. The
// job handle is deliberately never closed: Windows closes it at exit.
func bindChildProcess(cmd *exec.Cmd) error {
	childJobOnce.Do(func() {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			childJobErr = err
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(job)
			childJobErr = err
			return
		}
		childJob = job
	})
	if childJobErr != nil {
		return childJobErr
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(childJob, process)
}
