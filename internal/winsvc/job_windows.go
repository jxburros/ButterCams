//go:build windows

package winsvc

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job is a Windows Job Object.
type Job struct {
	h windows.Handle
}

// NewKillOnCloseJob creates a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:
// every process in it is terminated when the last handle to the job closes,
// which happens when the guardian exits for any reason (spec 4.1).
func NewKillOnCloseJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{h: h}, nil
}

// Assign puts a running process in the job.
func (j *Job) Assign(pid int) error {
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(p)
	return windows.AssignProcessToJobObject(j.h, p)
}

// Terminate kills every process in the job.
func (j *Job) Terminate() error {
	return windows.TerminateJobObject(j.h, 1)
}

// Close releases the handle. With kill-on-close this terminates the members
// that are still running.
func (j *Job) Close() {
	if j.h != 0 {
		windows.CloseHandle(j.h)
		j.h = 0
	}
}

// PrepareChild sets the process attributes for a supervised child: no console
// window and its own process group so console control events aimed at the
// guardian do not reach it.
func PrepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
