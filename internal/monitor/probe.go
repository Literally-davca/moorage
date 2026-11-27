// Package monitor runs uptime checks, keeps their state, opens incidents and
// sends alerts with context. Checks run on the hub, so they keep working when
// an agent is offline.
package monitor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/netguard"
	"moorage/internal/store"
)

// Check kinds.
const (
	KindHTTP  = "http"
	KindTCP   = "tcp"
	KindPing  = "ping"
	KindDNS   = "dns"
	KindStack = "stack"
	KindLog   = "log"
)

// Kinds lists the supported check kinds.
var Kinds = map[string]bool{KindHTTP: true, KindTCP: true, KindPing: true, KindDNS: true, KindStack: true, KindLog: true, KindResources: true}

// KindResources watches a host's disk and memory (M19M); Target is the host id.
const KindResources = "resources"

// Settings are the kind-specific options of a check (stored as JSON).
type Settings struct {
	Method         string `json:"method,omitempty"`        // http: GET or HEAD
	Status         string `json:"status,omitempty"`        // http: accepted codes, e.g. "200-299" or "200,301"
	Keyword        string `json:"keyword,omitempty"`       // http: text that must appear in the body
	KeywordAbsent  bool   `json:"keywordAbsent,omitempty"` // http: …or must not appear
	JSONPath       string `json:"jsonPath,omitempty"`      // http: e.g. status or data.items[0].ok
	JSONValue      string `json:"jsonValue,omitempty"`     // http: expected value at JSONPath
	IgnoreTLS      bool   `json:"ignoreTls,omitempty"`     // http: accept invalid certificates
	RecordType     string `json:"recordType,omitempty"`    // dns: A, AAAA, CNAME, MX, TXT, NS
	Resolver       string `json:"resolver,omitempty"`      // dns: server IP, empty = system resolver
	Expect         string `json:"expect,omitempty"`        // dns: an answer must contain this
	AllowLinkLocal bool   `json:"allowLinkLocal,omitempty"`
	// MaxLatencyMS turns a slow answer into a failure (http, tcp, ping, dns; 0 = off). With
	// several locations each location applies it to its own answer, so an agent outside the
	// home network can report that the service is slow from there.
	MaxLatencyMS  int    `json:"maxLatencyMs,omitempty"`
	Pattern       string `json:"pattern,omitempty"`       // log: substring or regex
	Regex         bool   `json:"regex,omitempty"`         // log: treat Pattern as a regex
	CaseSensitive bool   `json:"caseSensitive,omitempty"` // log: match case
	Threshold     int    `json:"threshold,omitempty"`     // log: matches in Window to alert (default 1)
	Window        int    `json:"window,omitempty"`        // log: sliding window in seconds (default 300)
	// URLs are the addresses of a multi-URL HTTP check (2+; one URL lives in Target only).
	URLs []string `json:"urls,omitempty"`
	// Stacks are the stacks of a multi-stack check (2+; one stack lives in Target only).
	Stacks []StackTarget `json:"stacks,omitempty"`
	// Host resources (M19M): used share in % that is a warning / goes down.
	DiskWarnPct int `json:"diskWarnPct,omitempty"`
	DiskCritPct int `json:"diskCritPct,omitempty"`
	MemCritPct  int `json:"memCritPct,omitempty"`
}

// LogFilter builds the host filter for a log check.
func (st Settings) LogFilter() engine.LogFilter {
	return engine.LogFilter{Pattern: st.Pattern, Regex: st.Regex, CaseSensitive: st.CaseSensitive}
}

// LogThreshold returns the configured match count (at least 1).
func (st Settings) LogThreshold() int {
	if st.Threshold < 1 {
		return 1
	}
	return st.Threshold
}

// LogWindow returns the configured sliding window duration (default 5 min).
func (st Settings) LogWindow() time.Duration {
	if st.Window < 10 {
		return 5 * time.Minute
	}
	return time.Duration(st.Window) * time.Second
}

// ParseSettings decodes stored settings, tolerating empty input.
func ParseSettings(s string) Settings {
	var st Settings
	if s != "" {
		json.Unmarshal([]byte(s), &st) //nolint:errcheck // validated on save
	}
	return st
}

// Outcome is the result of one probe.
type Outcome struct {
	OK      bool             `json:"ok"`
	Paused  bool             `json:"paused,omitempty"` // stack intentionally stopped: no alert
	Latency time.Duration    `json:"-"`
	Msg     string           `json:"msg"`
	Matches []engine.LogLine `json:"matches,omitempty"` // log check: recent matching lines
	// Unknown: a multi-location run where no location could run the check. It is
	// neither up nor down, so it never alerts and never recovers a check.
	Unknown bool `json:"unknown,omitempty"`
	// Locations are the per-location answers of a multi-location run (locations.go).
	Locations []LocationResult `json:"locations,omitempty"`
	// Targets are the per-target answers of a multi-target run (multitarget.go).
	Targets []TargetResult `json:"targets,omitempty"`
	// Degraded: some, but not all, targets of a multi-target check fail.
	Degraded bool `json:"degraded,omitempty"`
	// Quiet is set (maintenance or silenced) when every target of a multi-target check
	// sat the run out.
	Quiet string `json:"quiet,omitempty"`
	// cantRun: this system can't run the probe at all (no ICMP permission). A single
	// location reports it as a failure as before; in a multi-location run it is no vote.
	cantRun bool
}

// UserAgent identifies Moorage in HTTP checks.
var UserAgent = "Moorage uptime check"

// Probe runs one check once.
func Probe(ctx context.Context, c store.Check, reg *host.Registry) Outcome {
	st := ParseSettings(c.Settings)
	timeout := time.Duration(c.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	var out Outcome
	switch c.Kind {
	case KindHTTP:
		out = probeHTTP(ctx, c.Target, st, timeout)
	case KindTCP:
		out = probeTCP(ctx, c.Target, st, timeout)
	case KindPing:
		out = probePing(ctx, c.Target, st)
	case KindDNS:
		out = probeDNS(ctx, c.Target, st, timeout)
	case KindStack:
		out = probeStack(ctx, reg, c.HostID, c.Target)
	case KindLog:
		out = probeLogTail(ctx, reg, c.HostID, c.Stack, c.Target, st)
	case KindResources:
		out = probeResources(ctx, reg, c.Target, st)
	default:
		out = Outcome{Msg: "unknown check type " + c.Kind}
	}
	if out.Latency == 0 {
		out.Latency = time.Since(start)
	}
	if ctx.Err() != nil && !out.OK && !out.Paused {
		out.Msg = fmt.Sprintf("no answer within %s", timeout)
	}
	if RemoteKinds[c.Kind] {
		out.OK, out.Msg = LatencyLimit(st.MaxLatencyMS, out.OK, int(out.Latency.Milliseconds()), out.Msg)
	}
	return out
}

// LatencyLimit fails a successful answer that took longer than limitMS (0 = no limit).
// It is applied where the answer is measured and again on the hub for answers of older
// agents that don't know the setting; a failed answer stays as it is.
func LatencyLimit(limitMS int, ok bool, latencyMS int, msg string) (bool, string) {
	if limitMS <= 0 || !ok || latencyMS <= limitMS {
		return ok, msg
	}
	slow := fmt.Sprintf("slow: answered in %d ms, the limit is %d ms", latencyMS, limitMS)
	if msg != "" {
		slow += " (" + msg + ")"
	}
	return false, slow
}

func fail(format string, a ...any) Outcome { return Outcome{Msg: fmt.Sprintf(format, a...)} }

// ---- HTTP ------------------------------------------------------------------

const maxBody = 1 << 20

func probeHTTP(ctx context.Context, target string, st Settings, timeout time.Duration) Outcome {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fail("the URL must start with http:// or https://")
	}
	method := http.MethodGet
	if st.Method == http.MethodHead && st.Keyword == "" && st.JSONPath == "" {
		method = http.MethodHead
	}
	tr := &http.Transport{
		DialContext:           netguard.DialContext(timeout, st.AllowLinkLocal),
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: st.IgnoreTLS}, //nolint:gosec // opt-in per check
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		Proxy:                 nil,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("more than 5 redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return fail("%v", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	defer resp.Body.Close()
	latency := time.Since(start)
	if !statusOK(resp.StatusCode, st.Status) {
		return Outcome{Latency: latency, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	if st.Keyword != "" || st.JSONPath != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return Outcome{Latency: latency, Msg: "reading the response failed: " + cleanErr(err)}
		}
		if st.Keyword != "" {
			has := strings.Contains(string(body), st.Keyword)
			if has == st.KeywordAbsent {
				if st.KeywordAbsent {
					return Outcome{Latency: latency, Msg: fmt.Sprintf("HTTP %d, but %q is in the response", resp.StatusCode, st.Keyword)}
				}
				return Outcome{Latency: latency, Msg: fmt.Sprintf("HTTP %d, but %q isn't in the response", resp.StatusCode, st.Keyword)}
			}
		}
		if st.JSONPath != "" {
			got, err := jsonAt(body, st.JSONPath)
			if err != nil {
				return Outcome{Latency: latency, Msg: err.Error()}
			}
			if got != st.JSONValue {
				return Outcome{Latency: latency, Msg: fmt.Sprintf("%s is %q, expected %q", st.JSONPath, clip(got, 80), st.JSONValue)}
			}
		}
	}
	return Outcome{OK: true, Latency: latency, Msg: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

// statusOK matches a code against "200-299,301" (empty = 200-399).
func statusOK(code int, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return code >= 200 && code < 400
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b := a
		var err2 error
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 == nil && err2 == nil && code >= a && code <= b {
			return true
		}
	}
	return false
}

// ValidStatusSpec reports whether spec is a valid list of codes or ranges.
func ValidStatusSpec(spec string) bool {
	if strings.TrimSpace(spec) == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 100 || a > 599 {
			return false
		}
		if isRange {
			b, err := strconv.Atoi(strings.TrimSpace(hi))
			if err != nil || b < a || b > 599 {
				return false
			}
		}
	}
	return true
}

// jsonAt reads a value at a simple path like data.items[0].status.
func jsonAt(body []byte, path string) (string, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", errors.New("the response isn't valid JSON")
	}
	for _, seg := range splitPath(path) {
		switch cur := v.(type) {
		case map[string]any:
			next, ok := cur[seg]
			if !ok {
				return "", fmt.Errorf("%s isn't in the response", path)
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(cur) {
				return "", fmt.Errorf("%s isn't in the response", path)
			}
			v = cur[i]
		default:
			return "", fmt.Errorf("%s isn't in the response", path)
		}
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case nil:
		return "null", nil
	case json.Number, bool:
		return fmt.Sprint(t), nil
	default:
		b, _ := json.Marshal(t)
		return string(b), nil
	}
}

func splitPath(p string) []string {
	p = strings.ReplaceAll(p, "[", ".")
	p = strings.ReplaceAll(p, "]", "")
	var out []string
	for _, s := range strings.Split(strings.TrimPrefix(p, "$."), ".") {
		if s != "" && s != "$" {
			out = append(out, s)
		}
	}
	return out
}

// ---- TCP -------------------------------------------------------------------

func probeTCP(ctx context.Context, target string, st Settings, timeout time.Duration) Outcome {
	if _, _, err := net.SplitHostPort(target); err != nil {
		return fail("use host:port, for example db.lan:5432")
	}
	start := time.Now()
	conn, err := netguard.Dialer(timeout, st.AllowLinkLocal).DialContext(ctx, "tcp", target)
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	conn.Close()
	return Outcome{OK: true, Latency: time.Since(start), Msg: "port open"}
}

// ---- DNS -------------------------------------------------------------------

func probeDNS(ctx context.Context, name string, st Settings, timeout time.Duration) Outcome {
	r := net.DefaultResolver
	if st.Resolver != "" {
		server := st.Resolver
		if _, _, err := net.SplitHostPort(server); err != nil {
			server = net.JoinHostPort(server, "53")
		}
		d := netguard.Dialer(timeout, st.AllowLinkLocal)
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, server)
		}}
	}
	start := time.Now()
	var answers []string
	var err error
	switch strings.ToUpper(st.RecordType) {
	case "", "A", "AAAA":
		network := "ip4"
		if strings.ToUpper(st.RecordType) == "AAAA" {
			network = "ip6"
		}
		var ips []netip.Addr
		ips, err = r.LookupNetIP(ctx, network, name)
		for _, ip := range ips {
			answers = append(answers, ip.String())
		}
	case "CNAME":
		var c string
		c, err = r.LookupCNAME(ctx, name)
		answers = append(answers, c)
	case "MX":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, name)
		for _, m := range mx {
			answers = append(answers, fmt.Sprintf("%d %s", m.Pref, m.Host))
		}
	case "TXT":
		answers, err = r.LookupTXT(ctx, name)
	case "NS":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, name)
		for _, n := range ns {
			answers = append(answers, n.Host)
		}
	default:
		return fail("unsupported record type %s", st.RecordType)
	}
	latency := time.Since(start)
	if err != nil {
		return Outcome{Latency: latency, Msg: cleanErr(err)}
	}
	if len(answers) == 0 {
		return Outcome{Latency: latency, Msg: "no records"}
	}
	got := strings.Join(answers, ", ")
	if st.Expect != "" && !strings.Contains(got, st.Expect) {
		return Outcome{Latency: latency, Msg: fmt.Sprintf("answer %s doesn't contain %q", clip(got, 120), st.Expect)}
	}
	return Outcome{OK: true, Latency: latency, Msg: clip(got, 160)}
}

// ---- ping ------------------------------------------------------------------

func probePing(ctx context.Context, target string, st Settings) Outcome {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", target)
	if err != nil || len(ips) == 0 {
		return fail("can't resolve %s", target)
	}
	ip := ips[0]
	for _, a := range ips { // prefer IPv4, more often routable from containers
		if a.Is4() || a.Is4In6() {
			ip = a.Unmap()
			break
		}
	}
	if err := netguard.Check(ip, st.AllowLinkLocal); err != nil {
		return fail("%v", err)
	}
	rtt, err := ping(ctx, ip)
	if errors.Is(err, errNoICMP) {
		out := fail("%s", err.Error())
		out.cantRun = true
		return out
	}
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	return Outcome{OK: true, Latency: rtt, Msg: fmt.Sprintf("reply in %d ms", rtt.Milliseconds())}
}

// errNoICMP explains why ping can't run instead of a bare "operation not permitted".
var errNoICMP = errors.New("this system doesn't allow Moorage to send ping: run it as root (the Docker image does) " +
	"or allow ICMP sockets with sysctl net.ipv4.ping_group_range; a TCP check works everywhere")

// ping sends one ICMP echo. It tries an unprivileged ICMP socket first and
// falls back to a raw socket (needs CAP_NET_RAW, which Docker grants by default).
func ping(ctx context.Context, ip netip.Addr) (time.Duration, error) {
	v4 := ip.Is4()
	nets := [][2]string{{"udp6", "::"}, {"ip6:ipv6-icmp", "::"}}
	proto := 58
	var typ icmp.Type = ipv6.ICMPTypeEchoRequest
	if v4 {
		nets = [][2]string{{"udp4", "0.0.0.0"}, {"ip4:icmp", "0.0.0.0"}}
		proto = 1
		typ = ipv4.ICMPTypeEcho
	}
	var conn *icmp.PacketConn
	var err error
	var udp bool
	for _, n := range nets {
		if conn, err = icmp.ListenPacket(n[0], n[1]); err == nil {
			udp = strings.HasPrefix(n[0], "udp")
			break
		}
	}
	if err != nil {
		return 0, errNoICMP
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	conn.SetDeadline(deadline) //nolint:errcheck

	seq := int(time.Now().UnixNano() & 0x7fff)
	payload := []byte("moorage-" + strconv.Itoa(seq))
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: seq, Data: payload}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	var dst net.Addr = &net.IPAddr{IP: ip.AsSlice()}
	if udp {
		dst = &net.UDPAddr{IP: ip.AsSlice()}
	}
	start := time.Now()
	if _, err := conn.WriteTo(b, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, errors.New("no reply")
		}
		rm, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		echo, ok := rm.Body.(*icmp.Echo)
		if !ok || (rm.Type != ipv4.ICMPTypeEchoReply && rm.Type != ipv6.ICMPTypeEchoReply) {
			continue
		}
		// unprivileged sockets rewrite the ID, so match on the sequence and payload
		if echo.Seq == seq && string(echo.Data) == string(payload) {
			return time.Since(start), nil
		}
	}
}

// ---- stack -----------------------------------------------------------------

func probeStack(ctx context.Context, reg *host.Registry, hostID, stack string) Outcome {
	if reg == nil {
		return fail("hosts aren't available")
	}
	h, e, err := reg.Get(ctx, hostID)
	if errors.Is(err, host.ErrOffline) {
		return fail("host %s is offline", e.Name)
	}
	if err != nil {
		return fail("host not found")
	}
	s, err := h.GetStack(ctx, stack)
	if errors.Is(err, engine.ErrNotFound) {
		return fail("stack %s doesn't exist", stack)
	}
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	switch s.Status {
	case "up":
		return Outcome{OK: true, Msg: fmt.Sprintf("%d/%d running", s.Running, s.Total)}
	case "stopped", "inactive":
		return Outcome{Paused: true, Msg: "stack is stopped on purpose"}
	}
	var bad []string
	for _, c := range s.Containers {
		if c.State == "running" && c.Health != "unhealthy" {
			continue
		}
		name := c.Service
		if name == "" {
			name = c.Name
		}
		switch {
		case c.Health == "unhealthy":
			bad = append(bad, name+" is unhealthy")
		case c.ExitCode != nil:
			bad = append(bad, fmt.Sprintf("%s exited with code %d", name, *c.ExitCode))
		default:
			bad = append(bad, name+" is "+c.State)
		}
	}
	msg := fmt.Sprintf("%d of %d containers running", s.Running, s.Total)
	if len(bad) > 0 {
		msg += ": " + strings.Join(bad, ", ")
	}
	return Outcome{Msg: msg}
}

// ---- log -------------------------------------------------------------------

const (
	maxMatches      = 100
	maxMatchLineLen = 400
	maxAlertMatches = 10
	logTailSample   = 200
)

// probeLogStack verifies the host and stack of a log rule before evaluating its buffer.
func probeLogStack(ctx context.Context, reg *host.Registry, hostID, stack, container string) (ok bool, out Outcome) {
	if reg == nil {
		return false, fail("hosts aren't available")
	}
	h, e, err := reg.Get(ctx, hostID)
	if errors.Is(err, host.ErrOffline) {
		return false, fail("host %s is offline", e.Name)
	}
	if err != nil {
		return false, fail("host not found")
	}
	s, err := h.GetStack(ctx, stack)
	if errors.Is(err, engine.ErrNotFound) {
		return false, fail("stack %s doesn't exist", stack)
	}
	if err != nil {
		return false, fail("%s", cleanErr(err))
	}
	if s.Status == "stopped" || s.Status == "inactive" {
		return false, Outcome{Paused: true, Msg: "stack is stopped on purpose"}
	}
	if container != "" {
		found := false
		for _, c := range s.Containers {
			if c.ID == container || c.Name == container || c.Service == container {
				found = true
				break
			}
		}
		if !found {
			return false, fail("container %s doesn't exist in %s", container, stack)
		}
	}
	return true, Outcome{}
}

// probeLogTail tests a log check against the last logTailSample lines without starting a watcher.
func probeLogTail(ctx context.Context, reg *host.Registry, hostID, stack, container string, st Settings) Outcome {
	match, err := st.LogFilter().Compile()
	if err != nil {
		return fail("%s", err.Error())
	}
	if reg == nil {
		return fail("hosts aren't available")
	}
	h, e, err := reg.Get(ctx, hostID)
	if errors.Is(err, host.ErrOffline) {
		return fail("host %s is offline", e.Name)
	}
	if err != nil {
		return fail("host not found")
	}
	s, err := h.GetStack(ctx, stack)
	if errors.Is(err, engine.ErrNotFound) {
		return fail("stack %s doesn't exist", stack)
	}
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	allow := map[string]bool{}
	if container != "" {
		for _, c := range s.Containers {
			if c.ID == container || c.Name == container || c.Service == container {
				allow[c.Name] = true
			}
		}
		if len(allow) == 0 {
			return fail("container %s doesn't exist in %s", container, stack)
		}
	}
	lines, err := h.TailLogs(ctx, stack, logTailSample)
	if err != nil {
		return fail("%s", cleanErr(err))
	}
	var hits []engine.LogLine
	for _, l := range lines {
		if len(allow) > 0 && !allow[l.Container] {
			continue
		}
		if match(l.Message) {
			l.Message = clip(l.Message, maxMatchLineLen)
			hits = append(hits, l)
		}
	}
	total := len(lines)
	sample := hits
	if len(sample) > maxAlertMatches {
		sample = sample[len(sample)-maxAlertMatches:]
	}
	if len(hits) == 0 {
		return Outcome{OK: true, Msg: fmt.Sprintf("no matches of %q in the last %d lines", clip(st.Pattern, 40), total)}
	}
	word := "matches"
	if len(hits) == 1 {
		word = "match"
	}
	return Outcome{
		OK:      len(hits) < st.LogThreshold(),
		Msg:     fmt.Sprintf("%d %s of %q in the last %d lines: %s", len(hits), word, clip(st.Pattern, 40), total, clip(hits[0].Message, 120)),
		Matches: sample,
	}
}

// ---- helpers ---------------------------------------------------------------

func cleanErr(err error) string {
	var ge *netguard.Error
	if errors.As(err, &ge) {
		return ge.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"): // Linux / Windows wording
		return "connection refused"
	case strings.Contains(msg, "no such host"):
		return "the name doesn't resolve"
	case strings.Contains(msg, "certificate"):
		return "TLS certificate problem: " + msg
	}
	return clip(msg, 200)
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
