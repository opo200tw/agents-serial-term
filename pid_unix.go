//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// pidAlive reports whether a process with this pid exists. EPERM still
// means it exists (another user's process).
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
