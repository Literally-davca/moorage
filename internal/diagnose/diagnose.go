// Package diagnose explains why a container is not well (the Problems card, M18F): pure
// rules over the container's state (docker inspect), the tail of its log and what the hub
// knows (port map, recent changes, free disk, container events). Every problem says what
// is wrong, shows the evidence and offers fixes that are existing actions with a preview.
// Nothing here runs anything.
package diagnose

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Severities.
const (
	Error   = "error"
	Warning = "warning"
	Info    = "info"
)

// Facts is what the host reports about one container. Detailed is false when only the
// container list was available (an older agent): then OOMKilled, RestartCount, Health
// log and limits are unknown.
type Facts struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Service       string    `json:"service,omitempty"`
	Image         string    `json:"image"`
	State         string    `json:"state"` // running, exited, restarting, paused, created, dead
	ExitCode      int       `json:"exitCode"`
	OOMKilled     bool      `json:"oomKilled,omitempty"`
	Error         string    `json:"error,omitempty"` // State.Error
	RestartCount  int       `json:"restartCount,omitempty"`
	RestartPolicy string    `json:"restartPolicy,omitempty"` // no, always, unless-stopped, on-failure
	Health        string    `json:"health,omitempty"`        // healthy, unhealthy, starting
	HealthLog     []string  `json:"healthLog,omitempty"`     // last outputs, newest last
	MemLimit      int64     `json:"memLimit,omitempty"`      // bytes, 0 = none
	StartedAt     time.Time `json:"startedAt,omitzero"`
	FinishedAt    time.Time `json:"finishedAt,omitzero"`
	ImageArch     string    `json:"imageArch,omitempty"` // amd64, arm64…
	HostArch      string    `json:"hostArch,omitempty"`
	Detailed      bool      `json:"detailed"`
}

// Event is a recorded container event (container_events, shared with M18A).
type Event struct {
	Service  string
	Type     string // die, restart, health_status
	ExitCode *int
	Time     time.Time
	Count    int
}

// Change is something done to the stack (a history.git version, an image update).
type Change struct {
	Time    time.Time
	Message string
}

// Context is what the hub adds.
type Context struct {
	Logs        map[string][]string // container name -> last lines (only unhealthy containers)
	Events      []Event
	PortOwners  map[int]string // host port -> who holds it (stack/container or process)
	LastOpError string         // output of the last failed compose operation
	Changes     []Change       // newest first
	DiskFreePct float64        // free space on the Docker data disk, -1 = unknown
	Maintenance bool
	Now         time.Time
}

// Fix is an action the UI offers. Kinds map to existing actions: lint quick fixes
// (set-mem-limit, set-restart-no, use-free-port, add-env-var, add-healthcheck), restart,
// rollback (history / images), and links (ports, logs, terminal, disk, registries, deps).
type Fix struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Service string `json:"service,omitempty"`
	Value   string `json:"value,omitempty"`
}

// Problem is one finding.
type Problem struct {
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Container string    `json:"container"`
	Service   string    `json:"service,omitempty"`
	Title     string    `json:"title"`
	Detail    string    `json:"detail"`
	Evidence  []string  `json:"evidence,omitempty"`
	Fixes     []Fix     `json:"fixes,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	// DuringMaintenance marks problems seen inside a maintenance window.
	DuringMaintenance bool `json:"duringMaintenance,omitempty"`
	// Cause is the short line for notifications ("Out of memory (exit 137)").
	Cause string `json:"cause"`
}

// crash loop: this many restarts within the window.
const (
	loopRestarts = 3
	loopWindow   = 10 * time.Minute
	changeWindow = 15 * time.Minute
	maxEvidence  = 3
)

var (
	notFoundRe   = regexp.MustCompile(`(?i)(executable file not found|command not found|no such file or directory.*exec|exec: ".*": not found|: not found$)`)
	execFormatRe = regexp.MustCompile(`(?i)exec format error`)
	portInUseRe  = regexp.MustCompile(`(?i)(?:bind|listen).*?(?:0\.0\.0\.0|\[::\]|[\d.]+)?:(\d{1,5}).*(?:address already in use|port is already allocated)|port is already allocated|address already in use`)
	portNumRe    = regexp.MustCompile(`:(\d{2,5})\b`)
	missingVarRe = regexp.MustCompile(`(?i)(?:required variable|variable is not set|environment variable)\s+"?([A-Z_][A-Z0-9_]*)"?|\$\{?([A-Z_][A-Z0-9_]*)\}? (?:is )?(?:not set|missing|required)|missing required (?:env(?:ironment)? )?(?:var(?:iable)?)?\s*:?\s*([A-Z_][A-Z0-9_]*)`)
	permRe       = regexp.MustCompile(`(?i)permission denied`)
	permPathRe   = regexp.MustCompile(`(/[^\s'":,]+)`)
	pullNotFound = regexp.MustCompile(`(?i)manifest (?:for .* )?(?:unknown|not found)|not found: manifest unknown|pull access denied.*does not exist`)
	pullAuth     = regexp.MustCompile(`(?i)unauthorized|authentication required|denied: requested access`)
	pullLimit    = regexp.MustCompile(`(?i)toomanyrequests|rate limit`)
	noSpaceRe    = regexp.MustCompile(`(?i)no space left on device`)
	depDownRe    = regexp.MustCompile(`(?i)(connection refused|could not translate host name|name or service not known|no such host|getaddrinfo (?:ENOTFOUND|EAI_AGAIN)|temporary failure in name resolution)[^\n]*?([a-z0-9][a-z0-9_.-]*)?`)
	hostInMsgRe  = regexp.MustCompile(`(?i)(?:host(?:name)?|server|connect to|ENOTFOUND|resolve)\s+"?([a-z][a-z0-9_-]{1,62})"?`)
	healthTool   = regexp.MustCompile(`(?i)(curl|wget|nc|pg_isready|mysqladmin|redis-cli)[: ].*(not found|no such file)`)
)

// Diagnose returns the problems of the containers, most severe first. Healthy containers
// yield nothing.
func Diagnose(cs []Facts, c Context) []Problem {
	if c.Now.IsZero() {
		c.Now = time.Now()
	}
	var out []Problem
	for _, f := range cs {
		out = append(out, one(f, c)...)
	}
	rank := map[string]int{Error: 0, Warning: 1, Info: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

func one(f Facts, c Context) []Problem {
	logs := c.Logs[f.Name]
	p := Problem{Container: f.Name, Service: f.Service, DuringMaintenance: c.Maintenance, Since: f.FinishedAt}
	restarts := recentRestarts(f, c)
	unwell := f.State != "running" || f.Health == "unhealthy" || restarts >= loopRestarts
	if f.State == "running" && f.Health == "starting" {
		return []Problem{{Rule: "starting", Severity: Info, Container: f.Name, Service: f.Service, Title: "Starting…", Detail: "The healthcheck is still in its start period.", Cause: "Starting"}}
	}
	if !unwell || f.State == "paused" {
		return nil
	}
	if f.State == "exited" && f.ExitCode == 0 && restarts < loopRestarts && (f.RestartPolicy == "" || f.RestartPolicy == "no" || f.RestartPolicy == "on-failure") {
		return nil // a finished one-off job
	}
	evidence := func(lines ...string) []string {
		var ev []string
		for _, l := range lines {
			if l = strings.TrimSpace(l); l != "" && len(ev) < maxEvidence {
				ev = append(ev, truncate(l, 300))
			}
		}
		return ev
	}
	firstErr := firstErrorLine(logs)
	all := strings.Join(append(append([]string{f.Error, c.LastOpError}, logs...), f.HealthLog...), "\n")
	var out []Problem
	add := func(q Problem) {
		q.Container, q.Service, q.DuringMaintenance = p.Container, p.Service, p.DuringMaintenance
		if q.Since.IsZero() {
			q.Since = p.Since
		}
		out = append(out, q)
	}

	switch {
	// 1. out of memory
	case f.OOMKilled || (f.ExitCode == 137 && f.MemLimit > 0 && f.State != "running"):
		next := nextMem(f.MemLimit)
		q := Problem{Rule: "oom", Severity: Error, Title: "Out of memory",
			Detail: "The kernel killed the container because it used all the memory it may use.",
			Cause:  "Out of memory (exit 137)"}
		ev := fmt.Sprintf("exit %d", f.ExitCode)
		if f.MemLimit > 0 {
			ev += ", limit " + human(f.MemLimit)
		}
		if restarts > 1 {
			q.Detail = fmt.Sprintf("The kernel killed the container %d× in the last %d min.", restarts, int(loopWindow.Minutes()))
		}
		q.Evidence = evidence(ev)
		if f.MemLimit > 0 {
			q.Fixes = append(q.Fixes, Fix{Kind: "set-mem-limit", Label: "Raise memory limit to " + human(next), Service: f.Service, Value: memValue(next)})
		}
		add(q)
	// 3. command not found / wrong architecture
	case execFormatRe.MatchString(all) || (f.ImageArch != "" && f.HostArch != "" && f.ImageArch != f.HostArch && f.State != "running"):
		q := Problem{Rule: "wrong-arch", Severity: Error, Title: "Wrong architecture",
			Detail: fmt.Sprintf("The image is built for %s but the host is %s.", orUnknown(f.ImageArch), orUnknown(f.HostArch)),
			Cause:  "Image for the wrong architecture"}
		q.Evidence = evidence(match(all, execFormatRe))
		q.Fixes = []Fix{{Kind: "open-images", Label: "Choose another image variant", Service: f.Service}}
		add(q)
	case f.ExitCode == 126 || f.ExitCode == 127 || notFoundRe.MatchString(f.Error):
		q := Problem{Rule: "command-not-found", Severity: Error, Title: "Command not found",
			Detail: "The container's command or entrypoint doesn't exist in the image (exit " + strconv.Itoa(f.ExitCode) + ").",
			Cause:  fmt.Sprintf("Command not found (exit %d)", f.ExitCode)}
		q.Evidence = evidence(f.Error, match(all, notFoundRe), firstErr)
		q.Fixes = []Fix{{Kind: "open-compose", Label: "Edit command in compose.yaml", Service: f.Service}}
		add(q)
	// 9. image can't be pulled
	case pullNotFound.MatchString(c.LastOpError) || pullAuth.MatchString(c.LastOpError) || pullLimit.MatchString(c.LastOpError):
		q := Problem{Rule: "pull", Severity: Error, Title: "Image can't be pulled", Cause: "Image can't be pulled"}
		switch {
		case pullLimit.MatchString(c.LastOpError):
			q.Detail = "Docker Hub's pull rate limit was reached. Sign in to Docker Hub or wait."
			q.Fixes = []Fix{{Kind: "open-registries", Label: "Add registry login"}}
		case pullNotFound.MatchString(c.LastOpError):
			q.Detail = "The image tag doesn't exist in the registry."
			q.Fixes = []Fix{{Kind: "open-images", Label: "Choose an existing tag", Service: f.Service}}
		default:
			q.Detail = "The registry refused the pull: it needs a login."
			q.Fixes = []Fix{{Kind: "open-registries", Label: "Add registry login"}}
		}
		q.Evidence = evidence(match(c.LastOpError, pullNotFound, pullAuth, pullLimit))
		add(q)
	// 6. port already in use
	case portInUseRe.MatchString(all):
		port := portFrom(all)
		q := Problem{Rule: "port-in-use", Severity: Error, Title: "Port already in use", Cause: "Port already in use"}
		if port > 0 {
			q.Title = fmt.Sprintf("Port %d is already in use", port)
			if owner := c.PortOwners[port]; owner != "" {
				q.Title += " by " + owner
			}
			q.Fixes = append(q.Fixes, Fix{Kind: "use-free-port", Label: "Use a free port", Service: f.Service, Value: strconv.Itoa(port)})
		}
		q.Detail = "Another container or program on the host already listens on the port this service publishes."
		q.Evidence = evidence(match(all, portInUseRe))
		q.Fixes = append(q.Fixes, Fix{Kind: "open-ports", Label: "Open Ports"})
		add(q)
	// 10. no space left
	case noSpaceRe.MatchString(all) || (c.DiskFreePct >= 0 && c.DiskFreePct < 2 && f.State != "running"):
		q := Problem{Rule: "no-space", Severity: Error, Title: "No space left on device", Cause: "Disk full",
			Detail: "The disk is full, so the container can't write its data."}
		if c.DiskFreePct >= 0 {
			q.Evidence = append(q.Evidence, fmt.Sprintf("%.1f %% free", c.DiskFreePct))
		}
		q.Evidence = append(q.Evidence, evidence(match(all, noSpaceRe))...)
		q.Fixes = []Fix{{Kind: "open-disk", Label: "Clean up disk"}}
		add(q)
	// 7. missing variable
	case missingVarRe.MatchString(all):
		v := missingVar(all)
		q := Problem{Rule: "missing-variable", Severity: Error, Title: "Missing variable", Cause: "Missing variable",
			Detail: "The service needs an environment variable that isn't set."}
		if v != "" {
			q.Title = "Missing variable " + v
			q.Fixes = []Fix{{Kind: "add-env-var", Label: "Add " + v + " to .env", Value: v}}
		}
		q.Evidence = evidence(match(all, missingVarRe))
		add(q)
	// 8. permission denied
	case permRe.MatchString(all):
		line := match(all, permRe)
		q := Problem{Rule: "permission-denied", Severity: Error, Title: "Permission denied", Cause: "Permission denied",
			Detail: "The container's user can't read or write a path. Usually a volume owned by another user: set user: or PUID/PGID to the owner."}
		if m := permPathRe.FindString(line); m != "" {
			q.Title = "Permission denied on " + m
		}
		q.Evidence = evidence(line)
		q.Fixes = []Fix{{Kind: "open-terminal", Label: "Check owner in terminal", Service: f.Service}, {Kind: "open-compose", Label: "Set user / PUID in compose.yaml", Service: f.Service}}
		add(q)
	// 11. a dependency is down
	case depDownRe.MatchString(strings.Join(logs, "\n")):
		line := match(strings.Join(logs, "\n"), depDownRe)
		q := Problem{Rule: "dependency-down", Severity: Warning, Title: "A dependency is down", Cause: "Dependency is down",
			Detail: "This service can't reach another service it needs. Fix that one first."}
		if m := hostInMsgRe.FindStringSubmatch(line); m != nil {
			q.Title = "Can't reach " + m[1]
		}
		q.Evidence = evidence(line)
		q.Fixes = []Fix{{Kind: "open-deps", Label: "Open dependency graph"}}
		add(q)
	// 5. unhealthy
	case f.State == "running" && f.Health == "unhealthy":
		last := ""
		if n := len(f.HealthLog); n > 0 {
			last = f.HealthLog[n-1]
		}
		q := Problem{Rule: "unhealthy", Severity: Warning, Title: "Unhealthy", Cause: "Healthcheck failing",
			Detail: "The container runs but its healthcheck fails."}
		q.Evidence = evidence(last)
		if healthTool.MatchString(last) {
			q.Title = "Healthcheck tool missing"
			q.Detail = "The healthcheck calls a tool the image doesn't have. Use another healthcheck."
			q.Fixes = []Fix{{Kind: "add-healthcheck", Label: "Suggest another healthcheck", Service: f.Service}}
		}
		q.Fixes = append(q.Fixes, Fix{Kind: "open-logs", Label: "Open logs", Service: f.Service})
		add(q)
	// 4. finished with exit 0 but the policy restarts it
	case f.ExitCode == 0 && (f.RestartPolicy == "always" || f.RestartPolicy == "unless-stopped") && restarts >= loopRestarts:
		add(Problem{Rule: "oneshot-restarted", Severity: Warning, Title: "Finished, but the restart policy restarts it",
			Detail: "The container exits successfully and Docker starts it again. For a one-off job set restart: \"no\".",
			Cause:  "One-off job restarted", Evidence: []string{fmt.Sprintf("exit 0, restarted %d×", restarts)},
			Fixes: []Fix{{Kind: "set-restart-no", Label: `Set restart: "no"`, Service: f.Service}}})
	// 12. never started / dead
	case f.State == "created" || f.State == "dead":
		q := Problem{Rule: "not-started", Severity: Error, Title: "Never started", Cause: "Container never started",
			Detail: "Docker created the container but couldn't start it."}
		if f.State == "dead" {
			q.Title, q.Cause = "Dead", "Container is dead"
			q.Detail = "Docker couldn't stop or remove the container cleanly."
		}
		q.Evidence = evidence(f.Error, firstErr)
		q.Fixes = []Fix{{Kind: "restart", Label: "Restart", Service: f.Service}}
		add(q)
	default:
		// generic stop / crash
		title, cause := "Stopped", fmt.Sprintf("Exited with code %d", f.ExitCode)
		if f.State == "exited" && f.ExitCode != 0 {
			title = fmt.Sprintf("Crashed (exit %d)", f.ExitCode)
		}
		add(Problem{Rule: "exited", Severity: Error, Title: title, Cause: cause,
			Detail:   "The container stopped. The first error line of its last run is below.",
			Evidence: evidence(f.Error, firstErr),
			Fixes:    []Fix{{Kind: "open-logs", Label: "Open logs", Service: f.Service}, {Kind: "restart", Label: "Restart", Service: f.Service}}})
	}

	// 2. crash loop: says how often, the cause comes from the rules above
	if restarts >= loopRestarts && len(out) > 0 && out[0].Rule != "oom" {
		out[0].Title = fmt.Sprintf("%s – restarted %d× in %d min", out[0].Title, restarts, int(loopWindow.Minutes()))
		out[0].Cause = "Crash loop: " + out[0].Cause
		out[0].Severity = Error
	}
	// 13. started after a change
	if len(out) > 0 {
		if ch, ok := recentChange(out[0], c); ok {
			out[0].Evidence = append(out[0].Evidence, fmt.Sprintf("Started after %s at %s", ch.Message, ch.Time.Format("15:04")))
			out[0].Fixes = append(out[0].Fixes, Fix{Kind: "rollback", Label: "Roll back", Service: f.Service})
		}
	}
	if !f.Detailed {
		for i := range out {
			out[i].Evidence = append(out[i].Evidence, "Update the agent for a detailed diagnosis.")
		}
	}
	return out
}

func recentRestarts(f Facts, c Context) int {
	n := 0
	key := f.Service
	if key == "" {
		key = f.Name
	}
	for _, e := range c.Events {
		if e.Service != key || c.Now.Sub(e.Time) > loopWindow || (e.Type != "restart" && e.Type != "die") {
			continue
		}
		cnt := e.Count
		if cnt < 1 {
			cnt = 1
		}
		n += cnt
	}
	if f.State == "restarting" && n < loopRestarts && f.RestartCount >= loopRestarts {
		n = f.RestartCount
	}
	return n
}

func recentChange(p Problem, c Context) (Change, bool) {
	start := p.Since
	if start.IsZero() {
		start = c.Now
	}
	for _, ch := range c.Changes {
		if !ch.Time.After(start) && start.Sub(ch.Time) <= changeWindow {
			return ch, true
		}
	}
	return Change{}, false
}

var errLineRe = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|failed|cannot|can't|unable)\b`)

// firstErrorLine is the first line that looks like an error in the last run.
func firstErrorLine(lines []string) string {
	for _, l := range lines {
		if errLineRe.MatchString(l) {
			return l
		}
	}
	return ""
}

func match(s string, res ...*regexp.Regexp) string {
	for _, l := range strings.Split(s, "\n") {
		for _, re := range res {
			if re.MatchString(l) {
				return strings.TrimSpace(l)
			}
		}
	}
	return ""
}

func portFrom(s string) int {
	line := match(s, portInUseRe)
	ms := portNumRe.FindAllStringSubmatch(line, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(ms[i][1]); err == nil && n > 0 && n <= 65535 {
			return n
		}
	}
	return 0
}

func missingVar(s string) string {
	m := missingVarRe.FindStringSubmatch(s)
	for _, g := range m[1:] {
		if g != "" {
			return g
		}
	}
	return ""
}

// nextMem doubles the limit (at least +256 MiB).
func nextMem(limit int64) int64 {
	if limit <= 0 {
		return 512 << 20
	}
	n := limit * 2
	if n-limit < 256<<20 {
		n = limit + 256<<20
	}
	return n
}

func memValue(b int64) string {
	if b%(1<<30) == 0 {
		return strconv.FormatInt(b>>30, 10) + "g"
	}
	return strconv.FormatInt(b>>20, 10) + "m"
}

func human(b int64) string {
	if b >= 1<<30 && b%(1<<30) == 0 {
		return strconv.FormatInt(b>>30, 10) + " GiB"
	}
	if b >= 1<<30 {
		return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GiB"
	}
	return strconv.FormatInt(b>>20, 10) + " MiB"
}

func orUnknown(s string) string {
	if s == "" {
		return "another architecture"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
