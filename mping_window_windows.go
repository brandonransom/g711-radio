//go:build windows

package main

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
)

const createNewConsole = 0x00000010

// openPeerWindow opens a new console window that redisplays the status file
// until ctx ends.
func openPeerWindow(ctx context.Context, statusPath string) error {
	quoted := strings.ReplaceAll(statusPath, "'", "''")
	script := "$Host.UI.RawUI.WindowTitle='mping peers'; while($true){ $t = Get-Content -Raw -LiteralPath '" +
		quoted + "' -ErrorAction SilentlyContinue; Clear-Host; Write-Host $t; Start-Sleep 2 }"
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
