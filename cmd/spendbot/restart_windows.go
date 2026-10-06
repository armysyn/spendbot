//go:build windows

package main

import (
	"os"
	"os/exec"
)

// reexec starts the program now on disk (just updated) in this console and lets this process
// end; the new one waits for the port to be free (SPENDBOT_RESTARTED).
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "SPENDBOT_RESTARTED=1")
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
