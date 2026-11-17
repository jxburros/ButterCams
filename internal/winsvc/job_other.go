//go:build !windows

package winsvc

import (
	"os/exec"
	"sync"
	"syscall"
)

// Job is the development stand-in for a Windows Job Object: it tracks child
// process ids so they can be killed together.
type Job struct {
	mu   sync.Mutex
	pids map[int]struct{}
}

// NewKillOnCloseJob returns a Job. Outside Windows, children are tied to the
// guardian with PrepareChild instead.
func NewKillOnCloseJob() (*Job, error) {
	return &Job{pids: map[int]struct{}{}}, nil
}

// Assign records a child process.
func (j *Job) Assign(pid int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pids[pid] = struct{}{}
	return nil
}

// Terminate kills every recorded process.
func (j *Job) Terminate() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for pid := range j.pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	j.pids = map[int]struct{}{}
	return nil
}

// Close kills the members, mirroring kill-on-close.
func (j *Job) Close() { j.Terminate() }

// PrepareChild ties the child's lifetime to the guardian where the platform
// supports it (see job_linux.go).
func PrepareChild(cmd *exec.Cmd) { prepareChild(cmd) }

// SetLowPriority is a no-op on development systems.
func SetLowPriority(*exec.Cmd) {}
