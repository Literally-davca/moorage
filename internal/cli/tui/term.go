package tui

import (
	"bytes"
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"github.com/moby/term"
)

type Screen struct {
	fd    uintptr
	state *term.State
}

func EnterScreen() (*Screen, error) {
	fd := os.Stdin.Fd()
	st, err := term.SetRawTerminal(fd)
	if err != nil {
		return nil, fmt.Errorf("terminal raw mode: %w", err)
	}
	// Alternate buffer + hide cursor + clear screen
	_, _ = os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	return &Screen{fd: fd, state: st}, nil
}

func (s *Screen) Close() {
	if s == nil {
		return
	}
	_, _ = os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
	_ = term.RestoreTerminal(s.fd, s.state)
}

func (s *Screen) Size() (int, int) {
	ws, err := term.GetWinsize(os.Stdout.Fd())
	if err != nil || ws == nil || ws.Width == 0 || ws.Height == 0 {
		return 100, 30
	}
	return int(ws.Width), int(ws.Height)
}

func (s *Screen) Draw(frame string) {
	// Move home and emit lines with clear-to-EOL so redraws do not flicker
	var buf bytes.Buffer
	buf.WriteString("\x1b[H")
	buf.WriteString(frame)
	buf.WriteString("\x1b[J")
	_, _ = os.Stdout.Write(buf.Bytes())
}

// startKeyReader reads stdin bytes and decodes ANSI sequences into KeyEvents.
func startKeyReader(keys chan<- KeyEvent, done <-chan struct{}) {
	go func() {
		buf := make([]byte, 256)
		var pending []byte
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				var evs []KeyEvent
				evs, pending = decodeKeys(pending)
				for _, ev := range evs {
					select {
					case keys <- ev:
					case <-done:
						return
					}
				}
			}
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				return
			}
		}
	}()
}

func decodeKeys(b []byte) ([]KeyEvent, []byte) {
	var out []KeyEvent
	for len(b) > 0 {
		if b[0] == 0x03 { // Ctrl+C
			out = append(out, KeyEvent{Kind: KeyCtrlC})
			b = b[1:]
			continue
		}
		if b[0] == 0x12 { // Ctrl+R
			out = append(out, KeyEvent{Kind: KeyCtrlR})
			b = b[1:]
			continue
		}
		if b[0] == '\r' || b[0] == '\n' {
			out = append(out, KeyEvent{Kind: KeyEnter})
			b = b[1:]
			continue
		}
		if b[0] == '\t' {
			out = append(out, KeyEvent{Kind: KeyTab})
			b = b[1:]
			continue
		}
		if b[0] == 0x7f || b[0] == 0x08 {
			out = append(out, KeyEvent{Kind: KeyBackspace})
			b = b[1:]
			continue
		}
		if b[0] == 0x1b {
			if len(b) == 1 {
				out = append(out, KeyEvent{Kind: KeyEsc})
				b = b[1:]
				continue
			}
			if b[1] == '[' && len(b) >= 3 {
				switch b[2] {
				case 'A':
					out = append(out, KeyEvent{Kind: KeyUp})
					b = b[3:]
					continue
				case 'B':
					out = append(out, KeyEvent{Kind: KeyDown})
					b = b[3:]
					continue
				case 'Z': // Shift+Tab
					out = append(out, KeyEvent{Kind: KeyTab})
					b = b[3:]
					continue
				case '5':
					if len(b) >= 4 && b[3] == '~' {
						out = append(out, KeyEvent{Kind: KeyPageUp})
						b = b[4:]
						continue
					}
				case '6':
					if len(b) >= 4 && b[3] == '~' {
						out = append(out, KeyEvent{Kind: KeyPageDown})
						b = b[4:]
						continue
					}
				}
			}
			out = append(out, KeyEvent{Kind: KeyEsc})
			b = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			break
		}
		r, sz := utf8.DecodeRune(b)
		out = append(out, KeyEvent{Kind: KeyRune, Rune: r})
		b = b[sz:]
	}
	return out, b
}
