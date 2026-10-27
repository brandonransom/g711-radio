//go:build !windows && !linux

package main

import "syscall"

func setMpingSockopts(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
