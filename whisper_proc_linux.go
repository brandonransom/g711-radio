//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// prepareChildProcess asks the kernel to kill the child if g711-radio dies,
// so a crash can't leave whisper-server instances holding their ports.
func prepareChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func bindChildProcess(cmd *exec.Cmd) error { return nil }
