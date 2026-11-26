package config

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger from MOORAGE_LOG_LEVEL (debug, info, warn, error;
// default info) and MOORAGE_LOG_FORMAT (text, json; default text) (M18I). Unknown values
// fall back to the default and are returned as warnings to log once the logger exists.
func NewLogger(w io.Writer) (*slog.Logger, []string) {
	var warns []string
	level := slog.LevelInfo
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("MOORAGE_LOG_LEVEL"))); v {
	case "", "info":
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		warns = append(warns, "MOORAGE_LOG_LEVEL="+v+" is unknown (use debug, info, warn or error); using info")
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(w, opts)
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("MOORAGE_LOG_FORMAT"))); v {
	case "", "text":
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		warns = append(warns, "MOORAGE_LOG_FORMAT="+v+" is unknown (use text or json); using text")
	}
	return slog.New(h), warns
}
