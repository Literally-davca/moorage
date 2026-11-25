//go:build windows

package main

import "net"

// listenLocalSocket is a no-op on Windows where the CLI connects over --url + API token.
func listenLocalSocket(_ string) (net.Listener, func(), error) {
	return nil, func() {}, nil
}
