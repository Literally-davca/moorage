//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// listenLocalSocket opens a 0600 Unix domain socket for local CLI & TUI access.
func listenLocalSocket(path string) (net.Listener, func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, fmt.Errorf("create socket dir: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("%s already exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, nil, fmt.Errorf("chmod socket: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(path)
	}
	return ln, cleanup, nil
}
