//go:build !linux

package main

import "errors"

func runHostShellHelper(_ []string) error {
	return errors.New("host-shell-helper is supported on Linux only")
}
