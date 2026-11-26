package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"moorage/internal/monitor"
	"moorage/internal/notify"
)

// Gatus YAML config (config.yaml). Endpoints become checks, alerting providers become
// channels, and the endpoints that convert become one unpublished status page grouped
// like the Gatus dashboard.

type gatusConfig struct {
	Alerting          map[string]map[string]any `yaml:"alerting"`
	Endpoints         []gatusEndpoint           `yaml:"endpoints"`
	ExternalEndpoints []struct {
		Name string `yaml:"name"`
	} `yaml:"external-endpoints"`
	UI struct {
		Title  string `yaml:"title"`
		Header string `yaml:"header"`
	} `yaml:"ui"`
}

type gatusEndpoint struct {
	Name       string            `yaml:"name"`
	Group      string            `yaml:"group"`
	URL        string            `yaml:"url"`
	Method     string            `yaml:"method"`
	Interval   string            `yaml:"interval"`
	Enabled    *bool             `yaml:"enabled"`
	Conditions []string          `yaml:"conditions"`
	Headers    map[string]string `yaml:"headers"`
	Body       string            `yaml:"body"`
	GraphQL    bool              `yaml:"graphql"`
	DNS        *struct {
		QueryName string `yaml:"query-name"`
		QueryType string `yaml:"query-type"`
	} `yaml:"dns"`
	Client struct {
		Timeout  string `yaml:"timeout"`
		Insecure bool   `yaml:"insecure"`
	} `yaml:"client"`
	Alerts []struct {
		Type             string `yaml:"type"`
		Enabled          *bool  `yaml:"enabled"`
		FailureThreshold int    `yaml:"failure-threshold"`
	} `yaml:"alerts"`
}

// gatusKnownTop are top-level keys the importer reads; the rest is reported as not imported.
var gatusKnownTop = map[string]bool{"alerting": true, "endpoints": true, "external-endpoints": true, "ui": true}

// ParseGatus converts a Gatus YAML config into a plan.
func ParseGatus(data []byte) (*Plan, error) {
	if len(data) > MaxUpload {
		return nil, invalid("the file is larger than 5 MB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, invalid("the file is empty")
		}
		return nil, invalid("not valid YAML (%v)", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("the file holds more than one YAML document")
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, invalid("a Gatus config is a YAML mapping with an endpoints list")
	}
	var cfg gatusConfig
	if err := doc.Decode(&cfg); err != nil {
		return nil, invalid("not a Gatus config (%v)", err)
	}
	var top map[string]any
	if err := doc.Decode(&top); err != nil {
		return nil, invalid("not a Gatus config (%v)", err)
	}
	if _, ok := top["endpoints"]; !ok {
		return nil, invalid("not a Gatus config: no endpoints list")
	}
	if len(cfg.Endpoints) > MaxChecks {
		return nil, invalid("the config has %d endpoints; at most %d can be imported at once", len(cfg.Endpoints), MaxChecks)
	}
	if len(cfg.Alerting) > MaxChannels {
		return nil, invalid("too many alerting providers")
	}

	p := &Plan{Source: SourceGatus, Checks: []CheckItem{}, Channels: []ChannelItem{}, Pages: []PageItem{}, Skipped: []Skipped{}, Notes: []string{}}
	var ignored []string
	for k := range top {
		if !gatusKnownTop[k] {
			ignored = append(ignored, k)
		}
	}
	sort.Strings(ignored)
	if len(ignored) > 0 {
		p.Notes = append(p.Notes, "Not imported (no Moorage counterpart): "+strings.Join(ignored, ", ")+".")
	}

	// Providers in a stable order.
	provs := make([]string, 0, len(cfg.Alerting))
	for k := range cfg.Alerting {
		provs = append(provs, k)
	}
	sort.Strings(provs)
	chanKey := map[string]string{}
	defaults := map[string]map[string]any{}
	for _, prov := range provs {
		m := cfg.Alerting[prov]
		if d, ok := m["default-alert"].(map[string]any); ok {
			defaults[prov] = d
		}
		if it, ok := gatusChannel(p, prov, m); ok {
			chanKey[prov] = it.Key
			p.Channels = append(p.Channels, it)
		}
	}

	for _, ext := range cfg.ExternalEndpoints {
		p.skip("check", ext.Name, "External endpoints are pushed to Gatus by the service; Moorage has no counterpart.")
	}

	page := PageItem{Key: "page:gatus", Slug: "gatus", Title: "Status", Entries: []PageEntry{}}
	switch {
	case strings.TrimSpace(cfg.UI.Header) != "":
		page.Title = clip(cfg.UI.Header, 100)
	case strings.TrimSpace(cfg.UI.Title) != "":
		page.Title = clip(cfg.UI.Title, 100)
	}
	for i, ep := range cfg.Endpoints {
		it, ok := gatusCheck(p, i, ep)
		if !ok {
			continue
		}
		threshold := 0
		for _, a := range ep.Alerts {
			prov := strings.TrimSpace(a.Type)
			def := defaults[prov]
			on := true
			if v, ok := def["enabled"].(bool); ok {
				on = v
			}
			if a.Enabled != nil {
				on = *a.Enabled
			}
			if !on {
				continue
			}
			k, known := chanKey[prov]
			if !known {
				it.Warnings = append(it.Warnings, fmt.Sprintf("Alert %q isn't attached: that provider wasn't imported.", prov))
				continue
			}
			it.Channels = append(it.Channels, k)
			t := a.FailureThreshold
			if t <= 0 {
				if v, ok := def["failure-threshold"].(int); ok {
					t = v
				}
			}
			if t <= 0 {
				t = 3 // Gatus default
			}
			threshold = max(threshold, t)
		}
		if threshold > 0 {
			if threshold > 10 {
				it.Warnings = append(it.Warnings, fmt.Sprintf("Failure threshold %d is lowered to 10.", threshold))
			}
			it.Retries = clamp(threshold, 1, 10)
		}
		p.Checks = append(p.Checks, it)
	}
	uniqueNames(p)
	for _, c := range p.Checks {
		if len(page.Entries) == 100 {
			p.Notes = append(p.Notes, "The status page holds the first 100 endpoints only.")
			break
		}
		page.Entries = append(page.Entries, PageEntry{Section: clip(c.Group, 100), DisplayName: clip(c.Name, 100), CheckKey: c.Key})
	}
	if len(page.Entries) > 0 {
		page.Description = "Imported from Gatus."
		p.Pages = append(p.Pages, page)
		p.Notes = append(p.Notes, "The status page is created unpublished, so you can review it first.")
	}
	return p, nil
}

// conditionRe splits "[STATUS] == 200" and "[BODY].data.ok == true".
var conditionRe = regexp.MustCompile(`^\[([A-Z_]+)\]((?:\.[^\s=!<>]+)?)\s*(==|!=|<=|>=|<|>)\s*(.*)$`)

func gatusCheck(p *Plan, i int, ep gatusEndpoint) (CheckItem, bool) {
	name := clip(ep.Name, 60)
	if name == "" {
		name = fmt.Sprintf("Endpoint %d", i+1)
	}
	it := CheckItem{Key: fmt.Sprintf("check:%d", i), Name: name, Group: clip(ep.Group, 100), Channels: []string{}, Warnings: []string{}, Retries: defaultRetries}
	it.Enabled = ep.Enabled == nil || *ep.Enabled
	raw := strings.TrimSpace(ep.URL)
	if hasEnvRef(raw) {
		p.skip("check", name, "The URL uses an environment variable (${…}); fill it in and add the check by hand.")
		return it, false
	}
	interval := defaultInterval
	if ep.Interval != "" {
		d, err := time.ParseDuration(ep.Interval)
		if err != nil || d <= 0 {
			p.skip("check", name, "The interval "+ep.Interval+" can't be read.")
			return it, false
		}
		if d < 20*time.Second {
			it.Warnings = append(it.Warnings, "Interval raised to 20 seconds (the shortest Moorage allows).")
		}
		interval = int(d.Round(time.Second) / time.Second)
	}
	timeout := defaultTimeout
	if ep.Client.Timeout != "" {
		if d, err := time.ParseDuration(ep.Client.Timeout); err == nil && d > 0 {
			timeout = int((d + time.Second - 1) / time.Second)
		}
	}
	it.Interval, it.Timeout = timing(interval, timeout)

	if ep.DNS != nil {
		rt := strings.ToUpper(strings.TrimSpace(ep.DNS.QueryType))
		switch rt {
		case "", "A", "AAAA", "CNAME", "MX", "TXT", "NS":
		default:
			p.skip("check", name, "DNS record type "+rt+" isn't supported (A, AAAA, CNAME, MX, TXT or NS).")
			return it, false
		}
		it.Kind = monitor.KindDNS
		it.Target = strings.TrimSuffix(strings.TrimSpace(ep.DNS.QueryName), ".")
		it.Settings.RecordType = rt
		srv := strings.TrimPrefix(raw, "dns://")
		if h, _, err := net.SplitHostPort(srv); err == nil {
			srv = h
		}
		if net.ParseIP(srv) == nil {
			p.skip("check", name, "The DNS server "+raw+" isn't an IP address.")
			return it, false
		}
		it.Settings.Resolver = strings.TrimPrefix(raw, "dns://")
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" {
			p.skip("check", name, "The URL can't be read.")
			return it, false
		}
		switch u.Scheme {
		case "http", "https":
			method := strings.ToUpper(strings.TrimSpace(ep.Method))
			switch {
			case method != "" && method != "GET" && method != "HEAD":
				p.skip("check", name, fmt.Sprintf("HTTP method %s isn't supported (only GET and HEAD).", method))
				return it, false
			case len(ep.Headers) > 0 || strings.TrimSpace(ep.Body) != "" || ep.GraphQL:
				p.skip("check", name, "Uses custom headers, a request body or GraphQL, which Moorage HTTP checks don't send.")
				return it, false
			}
			it.Kind, it.Target = monitor.KindHTTP, raw
			if method == "HEAD" {
				it.Settings.Method = method
			}
			it.Settings.IgnoreTLS = ep.Client.Insecure
		case "tcp":
			if u.Hostname() == "" || u.Port() == "" {
				p.skip("check", name, "TCP endpoint without host:port.")
				return it, false
			}
			it.Kind, it.Target = monitor.KindTCP, u.Host
		case "icmp":
			it.Kind, it.Target = monitor.KindPing, u.Hostname()
		default:
			p.skip("check", name, fmt.Sprintf("Endpoints of type %s:// aren't supported.", u.Scheme))
			return it, false
		}
	}
	gatusConditions(&it, ep.Conditions)
	return it, true
}

// statusRange narrows the accepted status codes.
type statusRange struct {
	lo, hi int
	list   []int
	set    bool
}

func (s *statusRange) spec() string {
	if s.list != nil {
		parts := []string{}
		for _, c := range s.list {
			if c >= s.lo && c <= s.hi {
				parts = append(parts, strconv.Itoa(c))
			}
		}
		return strings.Join(parts, ",")
	}
	if s.lo > s.hi {
		return ""
	}
	if s.lo == s.hi {
		return strconv.Itoa(s.lo)
	}
	return fmt.Sprintf("%d-%d", s.lo, s.hi)
}

// gatusConditions turns the conditions a Moorage check can express into settings and
// warns about the rest (the check then tests less than Gatus did).
func gatusConditions(it *CheckItem, conds []string) {
	st := statusRange{lo: 100, hi: 599}
	dropped := func(c, why string) {
		it.Warnings = append(it.Warnings, fmt.Sprintf("Condition %q isn't converted: %s", clip(c, 80), why))
	}
	for _, raw := range conds {
		c := strings.TrimSpace(raw)
		m := conditionRe.FindStringSubmatch(c)
		if m == nil {
			dropped(c, "unsupported expression.")
			continue
		}
		ph, path, op, val := m[1], strings.TrimPrefix(m[2], "."), m[3], strings.TrimSpace(m[4])
		switch ph {
		case "CONNECTED":
			if op == "==" && val == "true" {
				continue // every Moorage check needs the connection
			}
			dropped(c, "unsupported comparison.")
		case "DNS_RCODE":
			if it.Kind == monitor.KindDNS && op == "==" && strings.EqualFold(val, "NOERROR") {
				continue
			}
			dropped(c, "only NOERROR is checked.")
		case "STATUS":
			if it.Kind != monitor.KindHTTP || path != "" {
				dropped(c, "status codes apply to HTTP endpoints only.")
				continue
			}
			if !applyStatus(&st, op, val) {
				dropped(c, "unsupported status comparison.")
				continue
			}
			st.set = true
		case "BODY":
			switch {
			case it.Kind == monitor.KindDNS && path == "" && op == "==" && !strings.Contains(val, "("):
				if it.Settings.Expect != "" {
					dropped(c, "only one expected answer is checked.")
					continue
				}
				it.Settings.Expect = val
			case it.Kind == monitor.KindHTTP && path == "" && (op == "==" || op == "!=") &&
				strings.HasPrefix(val, "pat(*") && strings.HasSuffix(val, "*)") &&
				!strings.ContainsAny(val[5:len(val)-2], "*?"):
				if it.Settings.Keyword != "" {
					dropped(c, "only one text match is checked.")
					continue
				}
				it.Settings.Keyword = val[5 : len(val)-2]
				it.Settings.KeywordAbsent = op == "!="
			case it.Kind == monitor.KindHTTP && path != "" && op == "==" && simpleJSONPath.MatchString(path) && !strings.Contains(val, "("):
				if it.Settings.JSONPath != "" {
					dropped(c, "only one JSON value is checked.")
					continue
				}
				it.Settings.JSONPath, it.Settings.JSONValue = path, val
			default:
				dropped(c, "unsupported body comparison.")
			}
		case "RESPONSE_TIME":
			dropped(c, "response time limits aren't checked; the timeout still applies.")
		case "CERTIFICATE_EXPIRATION", "DOMAIN_EXPIRATION":
			dropped(c, "expiry dates aren't checked.")
		default:
			dropped(c, "unsupported placeholder.")
		}
	}
	if st.set {
		spec := st.spec()
		switch {
		case spec == "":
			it.Warnings = append(it.Warnings, "The status conditions accept no code; 200–299 is used.")
		case spec != "200-299" && monitor.ValidStatusSpec(spec):
			it.Settings.Status = spec
		}
	}
}

func applyStatus(st *statusRange, op, val string) bool {
	if strings.HasPrefix(val, "any(") && strings.HasSuffix(val, ")") && op == "==" {
		var list []int
		for _, part := range strings.Split(val[4:len(val)-1], ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 100 || n > 599 {
				return false
			}
			list = append(list, n)
		}
		if st.list != nil || len(list) == 0 {
			return false
		}
		st.list = list
		return true
	}
	n, err := strconv.Atoi(val)
	if err != nil || n < 100 || n > 599 {
		return false
	}
	switch op {
	case "==":
		st.lo, st.hi = max(st.lo, n), min(st.hi, n)
	case "<":
		st.hi = min(st.hi, n-1)
	case "<=":
		st.hi = min(st.hi, n)
	case ">":
		st.lo = max(st.lo, n+1)
	case ">=":
		st.lo = max(st.lo, n)
	default:
		return false
	}
	return true
}

// gatusChannel maps one Gatus alerting provider to a channel.
func gatusChannel(p *Plan, prov string, m map[string]any) (ChannelItem, bool) {
	name := clip("Gatus "+prov, 60)
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case int:
			return strconv.Itoa(v)
		}
		return ""
	}
	it := ChannelItem{Key: "channel:" + prov, Name: name, Enabled: true, Warnings: []string{}}
	if _, ok := m["overrides"]; ok {
		it.Warnings = append(it.Warnings, "Group overrides aren't kept; every alert goes to the main destination.")
	}
	var c notify.Config
	switch prov {
	case "discord":
		it.Kind, c.URL = "discord", str("webhook-url")
	case "slack":
		it.Kind, c.URL = "slack", str("webhook-url")
	case "telegram":
		it.Kind, c.Token, c.ChatID = "telegram", str("token"), str("id")
		if str("api-url") != "" {
			it.Warnings = append(it.Warnings, "A custom Telegram API URL isn't kept.")
		}
	case "ntfy":
		server := strings.TrimRight(str("url"), "/")
		if server == "" {
			server = "https://ntfy.sh"
		}
		it.Kind, c.URL, c.Token = "ntfy", server+"/"+strings.TrimLeft(str("topic"), "/"), str("token")
	case "gotify":
		it.Kind, c.URL, c.Token = "gotify", str("server-url"), str("token")
	case "pushover":
		it.Kind, c.Token, c.ChatID = "pushover", str("application-token"), str("user-key")
	case "matrix":
		it.Kind, c.URL, c.Token, c.ChatID = "matrix", str("server-url"), str("access-token"), str("internal-room-id")
	case "email":
		it.Kind = "smtp"
		c.Host, c.Username, c.Password, c.From, c.To = str("host"), str("username"), str("password"), str("from"), str("to")
		c.Port, _ = strconv.Atoi(str("port"))
		c.Security = "starttls"
		if c.Port == 465 {
			c.Security = "tls"
		}
	case "custom":
		if method := strings.ToUpper(str("method")); method != "" && method != "POST" {
			p.skip("channel", name, "The custom alert uses "+method+"; Moorage webhooks always POST.")
			return it, false
		}
		it.Kind, c.URL = "webhook", str("url")
		it.Warnings = append(it.Warnings, "Moorage sends its own JSON payload; the custom body isn't kept.")
		if h, ok := m["headers"].(map[string]any); ok && len(h) > 0 {
			c.Headers = map[string]string{}
			for k, v := range h {
				if s, ok := v.(string); ok {
					c.Headers[k] = s
				}
			}
		}
	default:
		p.skip("channel", name, fmt.Sprintf("Moorage has no %q notification channel.", prov))
		return it, false
	}
	for _, v := range []string{c.URL, c.Token, c.ChatID, c.Password, c.Host, c.From, c.To} {
		if hasEnvRef(v) {
			p.skip("channel", name, "Uses an environment variable (${…}) for its settings; add the channel by hand.")
			return it, false
		}
	}
	it.Config = c
	it.Summary = channelSummary(it.Kind, c)
	return it, true
}

// uniqueNames gives repeated check names a suffix ("api (backend)", then "api (2)"),
// because imports are matched by name on a re-run. Channel names get the number suffix.
func uniqueNames(p *Plan) {
	seen := map[string]int{}
	for _, c := range p.Checks {
		seen[c.Name]++
	}
	used := map[string]bool{}
	for i := range p.Checks {
		c := &p.Checks[i]
		if seen[c.Name] > 1 && c.Group != "" {
			c.Name = clip(c.Name+" ("+c.Group+")", 60)
		}
		base, n := c.Name, 2
		for used[c.Name] {
			c.Name = clip(fmt.Sprintf("%s (%d)", base, n), 60)
			n++
		}
		used[c.Name] = true
	}
	usedCh := map[string]bool{}
	for i := range p.Channels {
		c := &p.Channels[i]
		base, n := c.Name, 2
		for usedCh[c.Name] {
			c.Name = clip(fmt.Sprintf("%s (%d)", base, n), 60)
			n++
		}
		usedCh[c.Name] = true
	}
}
