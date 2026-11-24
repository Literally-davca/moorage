//go:build !linux

package engine

import "errors"

func startLocalPTY(_ string, _, _ int) (TerminalSession, error) {
	return nil, errors.New("native host PTY is supported on Linux hosts only")
}
