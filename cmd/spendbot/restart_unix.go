//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reexec replaces this process with the program now on disk (just updated). The process id
// stays the same, so launchd, systemd or a terminal keep it as theirs.
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
