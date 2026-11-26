package engine

import (
	"os"
	"reflect"
	"testing"
)

func readFixture(t *testing.T, name, proto string) []NativePort {
	t.Helper()
	f, err := os.Open("testdata/procnet/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ParseProcNet(f, proto)
}

func TestParseProcNetTCP(t *testing.T) {
	got := readFixture(t, "tcp", "tcp")
	want := []NativePort{
		{IP: "", Port: 8080, Proto: "tcp"},
		{IP: "127.0.0.1", Port: 3306, Proto: "tcp"}, // duplicate line reported once
		{IP: "192.168.0.10", Port: 5080, Proto: "tcp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseProcNetTCP6(t *testing.T) {
	got := readFixture(t, "tcp6", "tcp")
	want := []NativePort{
		{IP: "", Port: 80, Proto: "tcp"},
		{IP: "::1", Port: 8081, Proto: "tcp"},
		{IP: "127.0.0.1", Port: 9090, Proto: "tcp"}, // v4-mapped
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseProcNetUDP(t *testing.T) {
	got := readFixture(t, "udp", "udp")
	want := []NativePort{{IP: "", Port: 53, Proto: "udp"}} // the connected client socket is left out
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseProcAddrRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "zz:0050", "0000:0050", "00000000", "00000000:XYZ"} {
		if _, _, ok := parseProcAddr(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
}
