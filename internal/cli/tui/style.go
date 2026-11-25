package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Moorage "calm harbor" ANSI 24-bit palette.
const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiReverse = "\x1b[7m"

	fgFoam  = "\x1b[38;2;241;245;249m"
	fgMist  = "\x1b[38;2;148;163;184m"
	fgHaze  = "\x1b[38;2;100;116;139m"
	fgBuoy  = "\x1b[38;2;56;189;248m"
	fgGreen = "\x1b[38;2;52;211;153m"
	fgAmber = "\x1b[38;2;251;191;36m"
	fgRed   = "\x1b[38;2;248;113;113m"
	bgSel   = "\x1b[48;2;30;41;59m"
	bgBar   = "\x1b[48;2;15;23;42m"
)

func padRight(s string, w int) string {
	if w <= 0 {
		return ""
	}
	sw := runewidth.StringWidth(s)
	if sw > w {
		return runewidth.Truncate(s, w, "…")
	}
	if sw == w {
		return s
	}
	return s + strings.Repeat(" ", w-sw)
}

func statusColor(status string) string {
	switch status {
	case "up", "running", "ok", "healthy":
		return fgGreen
	case "degraded", "pending", "warning", "maintenance", "restarting":
		return fgAmber
	case "down", "exited", "dead", "failed", "error":
		return fgRed
	case "stopped", "inactive", "paused":
		return fgHaze
	default:
		return fgMist
	}
}

func statusGlyph(status string) string {
	switch status {
	case "up", "running", "healthy":
		return "●"
	case "degraded", "pending", "maintenance", "restarting":
		return "◐"
	case "down", "exited", "dead", "failed":
		return "✖"
	default:
		return "○"
	}
}
