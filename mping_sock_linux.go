//go:build linux

package main

import "syscall"

const ipMulticastAll = 49 // IP_MULTICAST_ALL

// setMpingSockopts lets several sockets share the ping port and limits each
// to the groups it joined, so the group a ping was sent to is known.
func setMpingSockopts(fd uintptr) error {
	if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipMulticastAll, 0)
}
