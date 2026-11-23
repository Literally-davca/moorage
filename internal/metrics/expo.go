// Package metrics serves Moorage's state in the Prometheus text exposition format
// (version 0.0.4) on /metrics. It is written by hand so Moorage needs no client library.
package metrics

import (
	"bufio"
	"io"
	"math"
	"strconv"
	"strings"
)

// writer emits metric families. A family's # HELP and # TYPE lines are written only
// together with its first sample, so families without samples leave no trace, and a
// repeated label set inside one family is dropped (Prometheus rejects duplicate series).
type writer struct {
	w *bufio.Writer

	name, help, typ string
	headerDone      bool
	seen            map[string]bool
	buf             strings.Builder
}

func newWriter(w io.Writer) *writer {
	return &writer{w: bufio.NewWriter(w)}
}

// family starts a new metric family. typ is "gauge" or "counter".
func (w *writer) family(name, typ, help string) {
	w.name, w.typ, w.help = name, typ, help
	w.headerDone = false
	w.seen = map[string]bool{}
}

// sample writes one series of the current family. labels are name/value pairs.
func (w *writer) sample(value float64, labels ...string) {
	w.buf.Reset()
	w.buf.WriteString(w.name)
	if len(labels) > 0 {
		w.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.buf.WriteByte(',')
			}
			w.buf.WriteString(labels[i])
			w.buf.WriteString(`="`)
			w.buf.WriteString(escapeLabel(labels[i+1]))
			w.buf.WriteByte('"')
		}
		w.buf.WriteByte('}')
	}
	series := w.buf.String()
	if w.seen[series] {
		return
	}
	w.seen[series] = true
	if !w.headerDone {
		w.headerDone = true
		w.w.WriteString("# HELP " + w.name + " " + escapeHelp(w.help) + "\n") //nolint:errcheck // checked on flush
		w.w.WriteString("# TYPE " + w.name + " " + w.typ + "\n")              //nolint:errcheck
	}
	w.w.WriteString(series)             //nolint:errcheck
	w.w.WriteByte(' ')                  //nolint:errcheck
	w.w.WriteString(formatValue(value)) //nolint:errcheck
	w.w.WriteByte('\n')                 //nolint:errcheck
}

func (w *writer) flush() error { return w.w.Flush() }

// escapeLabel escapes a label value: backslash, double quote and line feed.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	return labelEscaper.Replace(s)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// escapeHelp escapes a HELP text: backslash and line feed.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
