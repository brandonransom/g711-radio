//go:build !windows

package main

import (
	"crypto/tls"
	"fmt"
	"log"
)

// buildTLSConfig is not supported on non-Windows platforms.
// Returns an error so the server falls back to HTTP.
func buildTLSConfig(_ string, _ *log.Logger) (*tls.Config, error) {
	return nil, fmt.Errorf("Windows certificate store not available on this platform")
}
