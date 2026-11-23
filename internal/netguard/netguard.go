// Package netguard keeps checks and notifications away from addresses that
// are dangerous to reach from a server: link-local ranges where cloud metadata
// services live (169.254.169.254 and friends), multicast and unspecified
// addresses. Private home-network ranges stay allowed, a homelab needs them.
//
// The check runs on the address actually dialed, after DNS resolution, so a
// hostname that later resolves to a metadata IP (DNS rebinding) is caught too.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

var metadata = []netip.Addr{
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
}

// Blocked returns why ip must not be contacted, or "".
func Blocked(ip netip.Addr) string {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid():
		return "invalid address"
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLinkLocalUnicast():
		return "link-local address (cloud metadata services live here)"
	case ip.IsMulticast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return "multicast address"
	}
	for _, m := range metadata {
		if ip == m {
			return "cloud metadata address"
		}
	}
	return ""
}

// Error is returned when a connection is refused by the guard.
type Error struct {
	Addr   string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("blocked %s: %s; allow link-local addresses on the check if this is intended", e.Addr, e.Reason)
}

// Dialer returns a dialer that refuses blocked addresses unless allow is true.
func Dialer(timeout time.Duration, allow bool) *net.Dialer {
	return &net.Dialer{
		Timeout:   timeout,
		KeepAlive: -1,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allow {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				host = address
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return &Error{Addr: host, Reason: "not an IP address"}
			}
			if r := Blocked(ip); r != "" {
				return &Error{Addr: ip.String(), Reason: r}
			}
			return nil
		},
	}
}

// Check validates an address resolved by other means (ICMP has no dialer).
func Check(ip netip.Addr, allow bool) error {
	if allow {
		return nil
	}
	if r := Blocked(ip); r != "" {
		return &Error{Addr: ip.String(), Reason: r}
	}
	return nil
}

// DialContext is a convenience for http.Transport.
func DialContext(timeout time.Duration, allow bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := Dialer(timeout, allow)
	return d.DialContext
}
