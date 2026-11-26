package engine

import (
	"context"
	"reflect"
	"testing"
)

func TestFormatParsePortString(t *testing.T) {
	cases := []struct {
		ip    string
		pub   int
		priv  int
		proto string
		want  string
	}{
		{"", 8080, 80, "tcp", "8080:80/tcp"},
		{"0.0.0.0", 8080, 80, "tcp", "8080:80/tcp"},
		{"::", 53, 53, "udp", "53:53/udp"},
		{"127.0.0.1", 5432, 5432, "tcp", "127.0.0.1:5432:5432/tcp"},
		{"fd00::1", 443, 8443, "", "[fd00::1]:443:8443/tcp"},
	}
	for _, c := range cases {
		got := FormatPortString(c.ip, c.pub, c.priv, c.proto)
		if got != c.want {
			t.Errorf("Format(%q) = %q, want %q", c.ip, got, c.want)
		}
		b, ok := ParsePortString(got)
		if !ok || b.Public != c.pub || b.Private != c.priv || b.IP != normalizeBindIP(c.ip) {
			t.Errorf("Parse(%q) = %+v %v", got, b, ok)
		}
	}
	for _, bad := range []string{"", "80", "x:80/tcp", "[::1:80:80/tcp", "70000:80/tcp"} {
		if _, ok := ParsePortString(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestBindIPsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "127.0.0.1", true},
		{"0.0.0.0", "10.0.0.1", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.1", "10.0.0.1", false},
		{"[::]", "::1", true},
	}
	for _, c := range cases {
		if got := BindIPsOverlap(c.a, c.b); got != c.want {
			t.Errorf("overlap(%q,%q) = %v", c.a, c.b, got)
		}
	}
}

func TestBuildPortReport(t *testing.T) {
	cs := []ContainerInfo{
		{Container: Container{Name: "web-app-1", Service: "app", State: "running", Ports: []string{"8080:80/tcp"}}, Project: "web", Managed: true},
		{Container: Container{Name: "db-1", Service: "db", State: "running", Ports: []string{"127.0.0.1:5432:5432/tcp"}}, Project: "web", Managed: true},
		{Container: Container{Name: "portainer", State: "running", Ports: []string{"9000:9000/tcp"}}, Standalone: true},
		{Container: Container{Name: "pihole", State: "running", HostNetwork: true}, Standalone: true},
	}
	stacks := []StackPorts{
		{Stack: "web", Ports: []DeclaredPort{{Service: "app", Port: 8080, Target: 80, Proto: "tcp"}}},       // running: not repeated
		{Stack: "media", Ports: []DeclaredPort{{Service: "jelly", Port: 8096, Target: 8096, Proto: "tcp"}}}, // stopped
	}
	native := []NativePort{
		{IP: "", Port: 8080, Proto: "tcp"}, // docker-proxy for web
		{IP: "", Port: 22, Proto: "tcp"},
		{IP: "", Port: 22, Proto: "tcp"},
	}
	rep := BuildPortReport(cs, stacks, native, NetModeHost)
	type row struct {
		Port   int
		Source string
		Owner  string
		State  string
	}
	var got []row
	for _, p := range rep.Ports {
		got = append(got, row{p.Port, p.Source, p.Owner(), p.State})
	}
	want := []row{
		{22, PortSourceHost, "a process on the host", PortStateListening},
		{5432, PortSourceStack, "web/db-1", PortStateRunning},
		{8080, PortSourceStack, "web/web-app-1", PortStateRunning},
		{8096, PortSourceCompose, "media/jelly", PortStateReserved},
		{9000, PortSourceContainer, "portainer", PortStateRunning},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !rep.Native || rep.NetMode != NetModeHost || !reflect.DeepEqual(rep.HostNetwork, []string{"pihole"}) {
		t.Fatalf("report meta: %+v", rep)
	}
	if bridge := BuildPortReport(cs, nil, nil, NetModeBridge); bridge.Native {
		t.Fatal("bridge mode must not claim native ports")
	}
}

func TestDeclaredPorts(t *testing.T) {
	compose := "services:\n" +
		"  app:\n" +
		"    image: nginx\n" +
		"    ports:\n" +
		"      - \"${WEB_PORT:-8080}:80\"\n" +
		"      - \"127.0.0.1:9000-9001:9000-9001/udp\"\n" +
		"  host:\n" +
		"    image: pihole\n" +
		"    network_mode: host\n"
	got := DeclaredPorts(context.Background(), "demo", compose, "WEB_PORT=8181\n")
	want := []DeclaredPort{
		{Service: "app", Port: 8181, Target: 80, Proto: "tcp"},
		{Service: "app", IP: "127.0.0.1", Port: 9000, Target: 9000, Proto: "udp"},
		{Service: "app", IP: "127.0.0.1", Port: 9001, Target: 9001, Proto: "udp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if DeclaredPorts(context.Background(), "bad", "services: [", "") != nil {
		t.Fatal("broken compose should yield nothing")
	}
}
