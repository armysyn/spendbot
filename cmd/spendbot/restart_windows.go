//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// createNewConsole is CREATE_NEW_CONSOLE: the process gets a console window of its own.
const createNewConsole = 0x00000010

// reexec starts the program now on disk (just updated) and lets this process end; the new one
// waits for the port to be free (SPENDBOT_RESTARTED). It gets a console window of its own:
// sharing this one, it died with it — start.bat ends when this process does, and Windows
// Terminal then closes the window and every process in it.
func reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "SPENDBOT_RESTARTED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
