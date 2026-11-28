package netguard

import (
	"net/netip"
	"testing"
)

func TestBlocked(t *testing.T) {
	for ip, blocked := range map[string]bool{
		"169.254.169.254":        true,
		"::ffff:169.254.169.254": true,
		"fd00:ec2::254":          true,
		"100.100.100.200":        true,
		"224.0.0.1":              true,
		"0.0.0.0":                true,
		"192.168.1.10":           false,
		"10.0.0.1":               false,
	} {
		if got := Blocked(netip.MustParseAddr(ip)) != ""; got != blocked {
			t.Errorf("Blocked(%s) = %v, want %v", ip, got, blocked)
		}
	}
}
