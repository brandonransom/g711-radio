//go:build !windows && !linux

package main

import "os/exec"

// On other platforms local whisper-server instances are stopped by
// whisperPool.Close on a normal shutdown only.
func prepareChildProcess(cmd *exec.Cmd) {}

func bindChildProcess(cmd *exec.Cmd) error { return nil }
