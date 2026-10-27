//go:build !windows

package main

import (
	"context"
	"errors"
)

func openPeerWindow(ctx context.Context, statusPath string) error {
	return errors.New("only supported on Windows; run `watch -n2 cat " + statusPath + "` in another terminal")
}
