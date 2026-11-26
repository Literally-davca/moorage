package tunnel

import (
	"net"

	"github.com/hashicorp/yamux"
)

// maxStreamWrite bounds how much one yamux stream hands to the shared session in one
// frame. yamux sends a write as a single frame of up to the stream window (256 KiB –
// 1 MiB here), and the session writes frames one after another over one connection, so
// a stream of bulk logs used to put up to a whole window in front of every small
// control request on a slow uplink (head-of-line blocking, measured in hol_test.go).
// With bounded writes the frames of all streams interleave every 16 KiB.
//
// This changes only how each side splits its own writes; the wire format is the same
// yamux framing, so it needs no negotiation and works with older agents and hubs.
const maxStreamWrite = 16 << 10

// boundedConn splits writes into frames of at most maxStreamWrite bytes.
type boundedConn struct{ net.Conn }

func (c boundedConn) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxStreamWrite {
			chunk = chunk[:maxStreamWrite]
		}
		w, err := c.Conn.Write(chunk)
		n += w
		if err != nil {
			return n, err
		}
		p = p[len(chunk):]
	}
	return n, nil
}

// boundedListener accepts yamux streams (the agent side) with bounded writes.
type boundedListener struct{ *yamux.Session }

func (l boundedListener) Accept() (net.Conn, error) {
	c, err := l.Session.Accept()
	if err != nil {
		return nil, err
	}
	return boundedConn{c}, nil
}

// openBounded opens a stream (the hub side) with bounded writes.
func openBounded(s *yamux.Session) (net.Conn, error) {
	c, err := s.Open()
	if err != nil {
		return nil, err
	}
	return boundedConn{c}, nil
}
