package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"

	"moorage/internal/monitor"
	"moorage/internal/notify"
)

// Uptime Kuma JSON backup (Settings → Backup → Export in Kuma 1.x). Only the fields the
// converter reads are modelled; Kuma has dozens more, which are ignored. Field types
// changed between Kuma versions (0/1 vs booleans, numbers vs strings), hence the flex types.

type kumaBackup struct {
	Version          string             `json:"version"`
	NotificationList []kumaNotification `json:"notificationList"`
	MonitorList      []kumaMonitor      `json:"monitorList"`
}

type kumaNotification struct {
	ID     flexInt   `json:"id"`
	Name   string    `json:"name"`
	Config string    `json:"config"` // JSON text with "type" and provider fields
	Active *flexBool `json:"active"`
}

type kumaMonitor struct {
	ID                  flexInt         `json:"id"`
	Name                string          `json:"name"`
	Type                string          `json:"type"`
	URL                 string          `json:"url"`
	Method              string          `json:"method"`
	Hostname            string          `json:"hostname"`
	Port                flexInt         `json:"port"`
	Interval            flexInt         `json:"interval"`
	Timeout             flexInt         `json:"timeout"`
	MaxRetries          flexInt         `json:"maxretries"`
	Active              *flexBool       `json:"active"`
	Keyword             string          `json:"keyword"`
	InvertKeyword       flexBool        `json:"invertKeyword"`
	IgnoreTLS           flexBool        `json:"ignoreTls"`
	UpsideDown          flexBool        `json:"upsideDown"`
	AcceptedStatusCodes []string        `json:"accepted_statuscodes"`
	DNSResolveType      string          `json:"dns_resolve_type"`
	DNSResolveServer    string          `json:"dns_resolve_server"`
	JSONPath            string          `json:"jsonPath"`
	ExpectedValue       string          `json:"expectedValue"`
	Headers             string          `json:"headers"`
	Body                string          `json:"body"`
	BasicAuthUser       string          `json:"basic_auth_user"`
	AuthMethod          string          `json:"authMethod"`
	NotificationIDList  map[string]bool `json:"notificationIDList"`
}

// flexInt accepts 5, 5.0, "5" and null.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e9 {
		return fmt.Errorf("not a number: %.20s", string(b))
	}
	*f = flexInt(math.Round(v))
	return nil
}

// flexBool accepts true/false, 0/1, "0"/"1"/"true"/"false" and null.
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(bytes.TrimSpace(b)), `"`) {
	case "true", "1":
		*f = true
	case "false", "0", "", "null":
		*f = false
	default:
		return fmt.Errorf("not a boolean: %.20s", string(b))
	}
	return nil
}

// ParseKuma converts an Uptime Kuma JSON backup into a plan.
func ParseKuma(data []byte) (*Plan, error) {
	if len(data) > MaxUpload {
		return nil, invalid("the file is larger than 5 MB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var b kumaBackup
	if err := dec.Decode(&b); err != nil {
		return nil, invalid("not an Uptime Kuma JSON backup (%v)", err)
	}
	if dec.More() {
		return nil, invalid("unexpected data after the JSON backup")
	}
	if b.MonitorList == nil && b.NotificationList == nil {
		return nil, invalid("not an Uptime Kuma JSON backup: no monitorList or notificationList")
	}
	if len(b.MonitorList) > MaxChecks {
		return nil, invalid("the backup has %d monitors; at most %d can be imported at once", len(b.MonitorList), MaxChecks)
	}
	if len(b.NotificationList) > MaxChannels {
		return nil, invalid("the backup has %d notifications; at most %d can be imported at once", len(b.NotificationList), MaxChannels)
	}
	p := &Plan{Source: SourceKuma, Checks: []CheckItem{}, Channels: []ChannelItem{}, Pages: []PageItem{}, Skipped: []Skipped{}, Notes: []string{}}
	p.Notes = append(p.Notes, "Uptime Kuma backups don't contain status pages, so none are imported. Add them under Monitoring → Status pages.")

	chanKey := map[int]string{}
	for _, n := range b.NotificationList {
		if it, ok := kumaChannel(p, n); ok {
			chanKey[int(n.ID)] = it.Key
			p.Channels = append(p.Channels, it)
		}
	}
	for _, m := range b.MonitorList {
		if it, ok := kumaCheck(p, m); ok {
			ids := make([]int, 0, len(m.NotificationIDList))
			for id, on := range m.NotificationIDList {
				if n, err := strconv.Atoi(id); err == nil && on {
					ids = append(ids, n)
				}
			}
			sort.Ints(ids)
			for _, id := range ids {
				if k, ok := chanKey[id]; ok {
					it.Channels = append(it.Channels, k)
				}
			}
			p.Checks = append(p.Checks, it)
		}
	}
	uniqueNames(p)
	return p, nil
}

func kumaCheck(p *Plan, m kumaMonitor) (CheckItem, bool) {
	name := clip(m.Name, 60)
	if name == "" {
		name = fmt.Sprintf("Monitor %d", m.ID)
	}
	typ := strings.ToLower(strings.TrimSpace(m.Type))
	it := CheckItem{Key: fmt.Sprintf("check:%d", m.ID), Name: name, Channels: []string{}, Warnings: []string{}}
	it.Interval, it.Timeout = timing(int(m.Interval), int(m.Timeout))
	it.Retries = clamp(int(m.MaxRetries)+1, 1, 10)
	it.Enabled = m.Active == nil || bool(*m.Active)

	if m.UpsideDown {
		p.skip("check", name, "Upside-down mode (up when the target fails) isn't supported.")
		return it, false
	}
	switch typ {
	case "http", "keyword", "json-query":
		if strings.TrimSpace(m.Headers) != "" && strings.TrimSpace(m.Headers) != "{}" ||
			strings.TrimSpace(m.Body) != "" || m.BasicAuthUser != "" ||
			(m.AuthMethod != "" && m.AuthMethod != "null") {
			p.skip("check", name, "Uses custom headers, a request body or authentication, which Moorage HTTP checks don't send.")
			return it, false
		}
		method := strings.ToUpper(strings.TrimSpace(m.Method))
		if method != "" && method != "GET" && method != "HEAD" {
			p.skip("check", name, fmt.Sprintf("HTTP method %s isn't supported (only GET and HEAD).", method))
			return it, false
		}
		it.Kind = monitor.KindHTTP
		it.Target = strings.TrimSpace(m.URL)
		if method == "HEAD" {
			it.Settings.Method = method
		}
		codes := strings.Join(m.AcceptedStatusCodes, ",")
		if codes != "" && codes != "200-299" {
			if monitor.ValidStatusSpec(codes) {
				it.Settings.Status = codes
			} else {
				it.Warnings = append(it.Warnings, "Accepted status codes "+codes+" couldn't be converted; 200–299 is used.")
			}
		}
		it.Settings.IgnoreTLS = bool(m.IgnoreTLS)
		switch typ {
		case "keyword":
			if m.Keyword == "" {
				p.skip("check", name, "Keyword monitor without a keyword.")
				return it, false
			}
			it.Settings.Keyword = clip(m.Keyword, 500)
			it.Settings.KeywordAbsent = bool(m.InvertKeyword)
		case "json-query":
			path := strings.TrimPrefix(strings.TrimSpace(m.JSONPath), "$.")
			if !simpleJSONPath.MatchString(path) || m.ExpectedValue == "" {
				p.skip("check", name, "The JSON query isn't a simple path (like data.status) with an expected value.")
				return it, false
			}
			it.Settings.JSONPath, it.Settings.JSONValue = path, m.ExpectedValue
		}
	case "port":
		h := strings.TrimSpace(m.Hostname)
		if h == "" || m.Port <= 0 || m.Port > 65535 {
			p.skip("check", name, "TCP monitor without a host or port.")
			return it, false
		}
		it.Kind = monitor.KindTCP
		it.Target = net.JoinHostPort(h, strconv.Itoa(int(m.Port)))
	case "ping":
		it.Kind = monitor.KindPing
		it.Target = strings.TrimSpace(m.Hostname)
	case "dns":
		rt := strings.ToUpper(strings.TrimSpace(m.DNSResolveType))
		switch rt {
		case "", "A", "AAAA", "CNAME", "MX", "TXT", "NS":
		default:
			p.skip("check", name, "DNS record type "+rt+" isn't supported (A, AAAA, CNAME, MX, TXT or NS).")
			return it, false
		}
		it.Kind = monitor.KindDNS
		it.Target = strings.TrimSpace(m.Hostname)
		it.Settings.RecordType = rt
		if srv := strings.TrimSpace(m.DNSResolveServer); srv != "" {
			if net.ParseIP(srv) == nil {
				it.Warnings = append(it.Warnings, "DNS server "+srv+" isn't an IP address; the system resolver is used.")
			} else {
				it.Settings.Resolver = srv
			}
		}
	case "group":
		p.skip("check", name, "Groups only arrange monitors in Kuma; the monitors inside are imported on their own.")
		return it, false
	case "docker":
		p.skip("check", name, "Docker container monitors aren't converted: add a Stack check for the stack instead.")
		return it, false
	case "push":
		p.skip("check", name, "Push monitors (the service calls Kuma) have no Moorage counterpart.")
		return it, false
	default:
		p.skip("check", name, fmt.Sprintf("Monitor type %q isn't supported.", typ))
		return it, false
	}
	return it, true
}

// kumaChannel maps Kuma notification providers to Moorage channel kinds.
func kumaChannel(p *Plan, n kumaNotification) (ChannelItem, bool) {
	name := clip(n.Name, 60)
	if name == "" {
		name = fmt.Sprintf("Notification %d", n.ID)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(n.Config), &cfg); err != nil {
		p.skip("channel", name, "Its settings couldn't be read.")
		return ChannelItem{}, false
	}
	str := func(k string) string {
		switch v := cfg[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	it := ChannelItem{Key: fmt.Sprintf("channel:%d", n.ID), Name: name, Enabled: n.Active == nil || bool(*n.Active), Warnings: []string{}}
	var c notify.Config
	typ := str("type")
	switch typ {
	case "discord":
		it.Kind, c.URL = "discord", str("discordWebhookUrl")
	case "slack":
		it.Kind, c.URL = "slack", str("slackwebhookURL")
	case "telegram":
		it.Kind, c.Token, c.ChatID = "telegram", str("telegramBotToken"), str("telegramChatID")
	case "ntfy":
		server := strings.TrimRight(str("ntfyserverurl"), "/")
		if server == "" {
			server = "https://ntfy.sh"
		}
		if str("ntfyusername") != "" && str("ntfyaccesstoken") == "" {
			p.skip("channel", name, "ntfy with a username and password isn't supported; use an access token.")
			return it, false
		}
		it.Kind, c.URL, c.Token = "ntfy", server+"/"+strings.TrimLeft(str("ntfytopic"), "/"), str("ntfyaccesstoken")
	case "gotify":
		it.Kind, c.URL, c.Token = "gotify", str("gotifyserverurl"), str("gotifyapplicationToken")
	case "pushover":
		it.Kind, c.Token, c.ChatID = "pushover", str("pushoverapptoken"), str("pushoveruserkey")
	case "matrix":
		it.Kind, c.URL, c.ChatID, c.Token = "matrix", str("homeserverUrl"), str("internalRoomId"), str("accessToken")
	case "smtp":
		it.Kind = "smtp"
		c.Host, c.Username, c.Password, c.From, c.To = str("smtpHost"), str("smtpUsername"), str("smtpPassword"), str("smtpFrom"), str("smtpTo")
		c.Port, _ = strconv.Atoi(str("smtpPort"))
		c.Security = "starttls"
		if v, _ := cfg["smtpSecure"].(bool); v {
			c.Security = "tls"
		}
		if str("smtpCC") != "" || str("smtpBCC") != "" {
			it.Warnings = append(it.Warnings, "CC and BCC recipients aren't kept.")
		}
	case "webhook":
		it.Kind, c.URL = "webhook", str("webhookURL")
		it.Warnings = append(it.Warnings, "Moorage sends its own JSON payload; a custom body or form format isn't kept.")
		if h := str("webhookAdditionalHeaders"); h != "" {
			var hdr map[string]string
			if json.Unmarshal([]byte(h), &hdr) != nil {
				it.Warnings = append(it.Warnings, "Additional headers couldn't be read and aren't kept.")
			} else if len(hdr) > 0 {
				c.Headers = hdr
			}
		}
	default:
		if typ == "" {
			typ = "unknown"
		}
		p.skip("channel", name, fmt.Sprintf("Moorage has no %q notification channel.", typ))
		return it, false
	}
	it.Config = c
	it.Summary = channelSummary(it.Kind, c)
	return it, true
}
