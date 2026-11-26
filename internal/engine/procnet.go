package engine

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// procNetFiles are read in native / host-network mode for the host's own sockets.
var procNetFiles = []struct{ path, proto string }{
	{"/proc/net/tcp", "tcp"}, {"/proc/net/tcp6", "tcp"},
	{"/proc/net/udp", "udp"}, {"/proc/net/udp6", "udp"},
}

// ReadNativePorts lists the TCP sockets in LISTEN state and the unconnected UDP sockets
// of this network namespace. Files that don't exist (not Linux) are skipped; the result
// is non-nil when at least one file could be read.
func ReadNativePorts() []NativePort {
	var out []NativePort
	read := false
	for _, f := range procNetFiles {
		fh, err := os.Open(f.path)
		if err != nil {
			continue
		}
		read = true
		out = append(out, ParseProcNet(fh, f.proto)...)
		fh.Close()
	}
	if read && out == nil {
		out = []NativePort{}
	}
	return out
}

// ParseProcNet parses one /proc/net/{tcp,tcp6,udp,udp6} table. proto is "tcp" or "udp".
// TCP keeps LISTEN (0A) sockets; UDP keeps sockets without a remote peer. Duplicate
// (ip, port) pairs are reported once; 0.0.0.0 and :: become "".
func ParseProcNet(r io.Reader, proto string) []NativePort {
	var out []NativePort
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first { // header: sl local_address rem_address st ...
			first = false
			if strings.Contains(sc.Text(), "local_address") {
				continue
			}
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		ip, port, ok := parseProcAddr(fields[1])
		if !ok || port == 0 {
			continue
		}
		state := fields[3]
		switch proto {
		case "tcp":
			if state != "0A" {
				continue
			}
		case "udp":
			rip, rport, ok := parseProcAddr(fields[2])
			if !ok || rport != 0 || !(rip == "") {
				continue
			}
		}
		k := ip + "|" + strconv.Itoa(port)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, NativePort{IP: ip, Port: port, Proto: proto})
	}
	return out
}

// parseProcAddr decodes "0100007F:1F90" (IPv4) or the 32 hex digit IPv6 form. The
// address is stored as 32-bit words in host byte order (little-endian on every
// platform Moorage ships for).
func parseProcAddr(s string) (string, int, bool) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	p, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return "", 0, false
	}
	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", 0, false
	}
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.BigEndian.PutUint32(b[i:], binary.LittleEndian.Uint32(raw[i:]))
	}
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return "", 0, false
	}
	return normalizeBindIP(a.Unmap().String()), int(p), true
}
