package engine

import (
	"reflect"
	"testing"
)

func TestInterfaceKind(t *testing.T) {
	cases := map[string]string{
		"eth0": AddrEthernet, "enp3s0": AddrEthernet, "eno1": AddrEthernet, "end0": AddrEthernet, "br0": AddrEthernet,
		"wlan0": AddrWiFi, "wlp2s0": AddrWiFi,
		"tailscale0": AddrVPN, "wg0": AddrVPN, "wt0": AddrVPN, "tun0": AddrVPN, "zt5u4y": AddrVPN,
		"lo": "", "docker0": "", "br-1a2b3c": "", "veth12ab": "", "virbr0": "", "cni0": "",
	}
	for name, want := range cases {
		if got := InterfaceKind(name); got != want {
			t.Errorf("%s: got %q want %q", name, got, want)
		}
	}
}

func TestClassifyInterfaces(t *testing.T) {
	in := []IfaceAddrs{
		{Name: "lo", Up: true, Loopback: true, Addrs: []string{"127.0.0.1/8", "::1/128"}},
		{Name: "tailscale0", Up: true, Addrs: []string{"100.87.100.2/32", "fd7a:115c:a1e0::2/128"}},
		{Name: "wlan0", Up: true, Addrs: []string{"192.168.1.50/24", "fe80::1/64"}},
		{Name: "eth0", Up: true, Addrs: []string{"fd00::10/64", "192.168.1.10/24"}},
		{Name: "eth1", Up: false, Addrs: []string{"10.0.0.5/24"}},
		{Name: "docker0", Up: true, Addrs: []string{"172.17.0.1/16"}},
		{Name: "veth9", Up: true, Addrs: []string{"169.254.1.1/16"}},
	}
	got := ClassifyInterfaces(in)
	var pairs [][2]string
	for _, a := range got {
		pairs = append(pairs, [2]string{a.Kind, a.Address})
		if !a.AutoDetected || a.ID == "" || a.Label == "" {
			t.Errorf("incomplete address %+v", a)
		}
	}
	want := [][2]string{
		{AddrEthernet, "192.168.1.10"}, {AddrEthernet, "fd00::10"},
		{AddrWiFi, "192.168.1.50"},
		{AddrVPN, "100.87.100.2"}, {AddrVPN, "fd7a:115c:a1e0::2"},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("got %v\nwant %v", pairs, want)
	}
	if got[3].Label != "Tailscale (tailscale0)" {
		t.Errorf("label %q", got[3].Label)
	}
}

func TestDetectedAddressesBridgeMode(t *testing.T) {
	var e Engine
	e.SetRuntimeNet(RuntimeNet{Mode: NetModeBridge})
	if e.DetectedAddresses() != nil {
		t.Fatal("bridge mode must not report interfaces of the container")
	}
	if (&Engine{}).NetMode() != NetModeBridge {
		t.Fatal("unknown mode should read as bridge")
	}
}
