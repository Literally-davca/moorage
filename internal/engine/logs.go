package engine

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// LogLine is one line of container output.
type LogLine struct {
	Time      string `json:"t"` // RFC3339Nano from Docker
	Container string `json:"c"`
	Project   string `json:"p,omitempty"`   // compose project name, empty for standalone
	Service   string `json:"svc,omitempty"` // compose service name, empty for standalone
	Stream    string `json:"s"`             // stdout | stderr
	Message   string `json:"m"`
}

// LogOptions selects containers and time bounds for QueryLogs.
type LogOptions struct {
	Stack     string // "" means across all containers on the host (compose + standalone)
	Container string // comma-separated IDs, names, or services; "" = all matching Stack
	Tail      int    // lines per container when > 0
	Since     string // RFC3339 or Unix seconds
	Until     string // RFC3339 or Unix seconds
	Follow    bool   // stream live lines until ctx ends
}

// maxLogLine cuts absurdly long lines so one container can't exhaust memory.
const maxLogLine = 16 * 1024

// MaxPatternLen caps log filter patterns.
const MaxPatternLen = 256

// LogFilter selects log lines on the host before they cross an agent tunnel.
type LogFilter struct {
	Pattern       string
	Regex         bool
	CaseSensitive bool
}

// Compile prepares a stateless line matcher for f.
func (f LogFilter) Compile() (func(string) bool, error) {
	if f.Pattern == "" {
		return nil, errors.New("enter a pattern to watch for")
	}
	if len([]rune(f.Pattern)) > MaxPatternLen {
		return nil, errors.New("the pattern can be at most 256 characters")
	}
	if f.Regex {
		pat := f.Pattern
		if !f.CaseSensitive {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}
	if f.CaseSensitive {
		return func(s string) bool { return strings.Contains(s, f.Pattern) }, nil
	}
	needle := strings.ToLower(f.Pattern)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), needle) }, nil
}

// matchesContainer reports whether c is wanted: empty means every container, otherwise
// want is one id, name or service, or several separated by commas (Docker names can't
// contain a comma), so the log view can follow a chosen set of containers.
func matchesContainer(c Container, want string) bool {
	if want == "" {
		return true
	}
	for _, w := range strings.Split(want, ",") {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if c.ID == w || c.Name == w || c.Service == w || (len(w) >= 12 && strings.HasPrefix(c.ID, w)) {
			return true
		}
	}
	return false
}

type logTarget struct {
	Container
	Project string
}

func (e *Engine) resolveLogTargets(ctx context.Context, stack, containerID string) ([]logTarget, error) {
	if stack != "" {
		byProject, _, err := e.projectContainers(ctx, stack)
		if err != nil {
			return nil, err
		}
		var out []logTarget
		for _, c := range byProject[stack] {
			if matchesContainer(c, containerID) {
				out = append(out, logTarget{Container: c, Project: stack})
			}
		}
		return out, nil
	}
	all, err := e.AllContainers(ctx)
	if err != nil {
		return nil, err
	}
	var out []logTarget
	for _, ci := range all {
		if matchesContainer(ci.Container, containerID) {
			out = append(out, logTarget{Container: ci.Container, Project: ci.Project})
		}
	}
	return out, nil
}

// StreamLogs follows the logs of every container of a stack (or one container, or across
// the host when stack == "") and calls emit for each line until ctx ends.
func (e *Engine) StreamLogs(ctx context.Context, stack, containerID string, tail int, emit func(LogLine)) error {
	return e.QueryLogs(ctx, LogOptions{
		Stack:     stack,
		Container: containerID,
		Tail:      tail,
		Follow:    true,
	}, emit)
}

// QueryLogs streams or fetches container logs with optional Since/Until bounds and
// host-wide container selection. Non-follow queries across multiple containers are merged
// in chronological order with bounded per-container channel buffering.
func (e *Engine) QueryLogs(ctx context.Context, opts LogOptions, emit func(LogLine)) error {
	targets, err := e.resolveLogTargets(ctx, opts.Stack, opts.Container)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errors.New("no containers")
	}
	if opts.Follow || len(targets) == 1 {
		var wg sync.WaitGroup
		for _, t := range targets {
			wg.Add(1)
			go func(t logTarget) {
				defer wg.Done()
				if err := e.containerLogsOpts(ctx, t.Container, t.Project, opts.Tail, opts.Since, opts.Until, opts.Follow, emit); err != nil && ctx.Err() == nil {
					e.log.Debug("log stream ended", "container", t.Name, "err", err)
				}
			}(t)
		}
		wg.Wait()
		if opts.Follow {
			return ctx.Err()
		}
		return nil
	}

	// K-way merge for historical / download queries across multiple containers so output is
	// ordered by timestamp without buffering entire multi-megabyte logs in memory.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chans := make([]chan LogLine, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		ch := make(chan LogLine, 64)
		chans[i] = ch
		wg.Add(1)
		go func(t logTarget, ch chan LogLine) {
			defer wg.Done()
			defer close(ch)
			_ = e.containerLogsOpts(ctx, t.Container, t.Project, opts.Tail, opts.Since, opts.Until, false, func(l LogLine) {
				select {
				case ch <- l:
				case <-ctx.Done():
				}
			})
		}(t, ch)
	}
	h := &logMergeHeap{}
	heap.Init(h)
	for i, ch := range chans {
		if l, ok := <-ch; ok {
			heap.Push(h, logHeapItem{line: l, idx: i})
		}
	}
	for h.Len() > 0 {
		if ctx.Err() != nil {
			break
		}
		top := heap.Pop(h).(logHeapItem)
		emit(top.line)
		if next, ok := <-chans[top.idx]; ok {
			heap.Push(h, logHeapItem{line: next, idx: top.idx})
		}
	}
	wg.Wait()
	return ctx.Err()
}

type logHeapItem struct {
	line LogLine
	idx  int
}

type logMergeHeap []logHeapItem

func (h logMergeHeap) Len() int { return len(h) }
func (h logMergeHeap) Less(i, j int) bool {
	if h[i].line.Time == h[j].line.Time {
		return h[i].idx < h[j].idx
	}
	return logTimeLess(h[i].line.Time, h[j].line.Time)
}
func (h logMergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *logMergeHeap) Push(x any)   { *h = append(*h, x.(logHeapItem)) }
func (h *logMergeHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// watchPoll is how often WatchLogs looks for new, restarted or exited containers.
var watchPoll = 5 * time.Second

// WatchLogs follows only new log lines of a stack (or one container/service)
// that match f, without replaying history, until ctx ends or listing fails.
//
// Containers come and go (restarts, re-creates on deploy, crash loops), so the
// container list is polled every watchPoll. Each container is read from the
// later of the watch start and its last seen line, so a reconnect neither
// replays nor duplicates lines. A container that is not running is read once
// without following whenever its state or start time changed, so a run shorter
// than watchPoll (a crash right after start) is still read to its last line.
func (e *Engine) WatchLogs(ctx context.Context, stack, containerID string, f LogFilter, emit func(LogLine)) error {
	match, err := f.Compile()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := time.Now()

	var (
		mu       sync.Mutex
		lastSeen = map[string]time.Time{} // container ID -> time of its newest line read
		atLast   = map[string]int{}       // container ID -> lines read with exactly that time
		busy     = map[string]bool{}      // containers being read right now
		state    = map[string]string{}    // container state + start time at the previous poll
		wg       sync.WaitGroup
		ended    = make(chan struct{}, 1)
	)
	defer wg.Wait()
	inclusive := e.isPodman(ctx)
	// sinceFor returns where to resume a container and the lines at that exact time that
	// were already read. Docker has nanosecond timestamps, so resuming 1 ns after the last
	// line is exact. Podman's timestamps are whole seconds (v4.9 formats them with
	// time.RFC3339) and its since filter is inclusive and parsed as a float, so it resumes
	// at the last line's time and the lines already read at that time are skipped.
	sinceFor := func(id string) (string, time.Time, int) {
		mu.Lock()
		defer mu.Unlock()
		t := start
		last, ok := lastSeen[id]
		if ok && last.After(t) {
			t = last.Add(time.Nanosecond)
			if inclusive {
				t = last
			}
		}
		return t.UTC().Format(time.RFC3339Nano), last, atLast[id]
	}
	read := func(c Container, follow bool) {
		defer wg.Done()
		since, last, skip := sinceFor(c.ID)
		err := e.containerLogs(ctx, c, 0, since, follow, func(l LogLine) {
			if t, err := time.Parse(time.RFC3339Nano, l.Time); err == nil {
				mu.Lock() // stdout and stderr lines arrive on two goroutines
				if !last.IsZero() && (t.Before(last) || (t.Equal(last) && skip > 0)) {
					if t.Equal(last) {
						skip--
					}
					mu.Unlock()
					return // already read before the stream was resumed
				}
				switch {
				case t.After(lastSeen[c.ID]):
					lastSeen[c.ID] = t
					atLast[c.ID] = 1
				case t.Equal(lastSeen[c.ID]):
					atLast[c.ID]++
				}
				mu.Unlock()
			}
			if match(l.Message) {
				emit(l)
			}
		})
		if err != nil && ctx.Err() == nil {
			e.log.Debug("log watch ended", "container", c.Name, "err", err)
		}
		mu.Lock()
		delete(busy, c.ID)
		mu.Unlock()
		if follow {
			select {
			case ended <- struct{}{}:
			default:
			}
		}
	}
	poll := func() error {
		byProject, _, err := e.projectContainers(ctx, stack)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, c := range byProject[stack] {
			if !matchesContainer(c, containerID) {
				continue
			}
			present[c.ID] = true
			key := c.State
			if c.State != "running" {
				// the list shows "exited" before and after a short run; the start time tells them apart
				if insp, err := e.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); err == nil && insp.Container.State != nil {
					key += " " + insp.Container.State.StartedAt
				}
			}
			mu.Lock()
			changed := state[c.ID] != key
			state[c.ID] = key
			switch {
			case busy[c.ID]:
				// a follow stream ends by itself when the container stops
			case c.State == "running":
				busy[c.ID] = true
				wg.Add(1)
				go read(c, true)
			case changed:
				busy[c.ID] = true
				wg.Add(1)
				go read(c, false)
			}
			mu.Unlock()
		}
		mu.Lock()
		for id := range state { // forget removed containers
			if !present[id] && !busy[id] {
				delete(lastSeen, id)
				delete(atLast, id)
				delete(state, id)
			}
		}
		mu.Unlock()
		return nil
	}

	if err := poll(); err != nil {
		return err
	}
	tick := time.NewTicker(watchPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ended:
			// pick the container up again soon, but never spin on a stream that fails at once
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		case <-tick.C:
		}
		if err := poll(); err != nil {
			return err
		}
	}
}

// TailLogs returns the last n lines of every container of a stack, running or
// not, merged by time. Used as context in alerts.
func (e *Engine) TailLogs(ctx context.Context, stack string, n int) ([]LogLine, error) {
	byProject, _, err := e.projectContainers(ctx, stack)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var all []LogLine
	var wg sync.WaitGroup
	for _, c := range byProject[stack] {
		wg.Add(1)
		go func(c Container) {
			defer wg.Done()
			var lines []LogLine
			var lmu sync.Mutex
			_ = e.containerLogs(ctx, c, n, "", false, func(l LogLine) { lmu.Lock(); lines = append(lines, l); lmu.Unlock() })
			lmu.Lock()
			mu.Lock()
			all = append(all, lines...)
			mu.Unlock()
			lmu.Unlock()
		}(c)
	}
	wg.Wait()
	sort.SliceStable(all, func(i, j int) bool { return logTimeLess(all[i].Time, all[j].Time) })
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

func (e *Engine) containerLogs(ctx context.Context, c Container, tail int, since string, follow bool, emit func(LogLine)) error {
	return e.containerLogsOpts(ctx, c, "", tail, since, "", follow, emit)
}

func (e *Engine) containerLogsOpts(ctx context.Context, c Container, project string, tail int, since, until string, follow bool, emit func(LogLine)) error {
	insp, err := e.api.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	tty := insp.Container.Config != nil && insp.Container.Config.Tty
	opts := client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Timestamps: true,
		Since:      since,
		Until:      until,
	}
	if tail > 0 {
		opts.Tail = strconv.Itoa(tail)
	} else if since == "" && until == "" {
		opts.Tail = "all"
	}
	rc, err := e.api.ContainerLogs(ctx, c.ID, opts)
	if err != nil {
		return err
	}
	defer rc.Close()

	taggedEmit := func(l LogLine) {
		l.Project = project
		l.Service = c.Service
		emit(l)
	}
	out := lineEmitter(c.Name, "stdout", taggedEmit)
	if tty {
		_, err = io.Copy(out, rc)
		out.Close()
		return err
	}
	errw := lineEmitter(c.Name, "stderr", taggedEmit)
	_, err = stdcopy.StdCopy(out, errw, rc)
	out.Close()
	errw.Close()
	return err
}

// lineEmitter turns a byte stream into LogLines.
func lineEmitter(name, stream string, emit func(LogLine)) io.WriteCloser {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			ts, msg, ok := strings.Cut(line, " ")
			if !ok {
				ts, msg = "", line
			}
			ts = utcTimestamp(ts)
			if len(msg) > maxLogLine {
				msg = msg[:maxLogLine] + " …"
			}
			emit(LogLine{Time: ts, Container: name, Stream: stream, Message: strings.TrimRight(msg, "\r")})
		}
		io.Copy(io.Discard, pr) //nolint:errcheck // drain after a scanner error
	}()
	return &waitCloser{PipeWriter: pw, done: done}
}

// utcTimestamp rewrites a log timestamp with a zone offset into UTC. Docker sends UTC
// ("…Z") already; Podman sends local time with an offset ("2024-05-01T10:00:00+02:00",
// pkg/api/handlers/compat/containers_logs.go), which would sort wrongly next to UTC times.
func utcTimestamp(ts string) string {
	if ts == "" || strings.HasSuffix(ts, "Z") {
		return ts
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// logTimeLess orders log timestamps by time. Comparing the strings is wrong when one
// has fractional seconds and the other doesn't ("…00.5Z" sorts before "…00Z").
func logTimeLess(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339Nano, a)
	tb, eb := time.Parse(time.RFC3339Nano, b)
	if ea != nil || eb != nil {
		return a < b
	}
	return ta.Before(tb)
}

// waitCloser's Close returns only after every line has been emitted.
type waitCloser struct {
	*io.PipeWriter
	done chan struct{}
}

func (w *waitCloser) Close() error {
	err := w.PipeWriter.Close()
	<-w.done
	return err
}

func itoa(n int) string {
	if n <= 0 {
		return "all"
	}
	return strconv.Itoa(n)
}

// Stat is one resource sample for a container.
type Stat struct {
	ID       string  `json:"id"`
	CPU      float64 `json:"cpu"` // percent of one core, like docker stats (200 = two cores busy)
	MemUsed  uint64  `json:"memUsed"`
	MemLimit uint64  `json:"memLimit"` // 0 = unknown
	// CPUNote and MemNote say why the engine gave no value; the UI then shows "–" with
	// the reason instead of a 0. Older hubs ignore them.
	CPUNote string `json:"cpuNote,omitempty"`
	MemNote string `json:"memNote,omitempty"`
}

// Stats samples CPU and memory of the running containers of a stack (takes about 1 s).
func (e *Engine) Stats(ctx context.Context, stack string) ([]Stat, error) {
	byProject, _, err := e.projectContainers(ctx, stack)
	if err != nil {
		return nil, err
	}
	var (
		mu  sync.Mutex
		out []Stat
		wg  sync.WaitGroup
	)
	podman := e.isPodman(ctx)
	for _, c := range byProject[stack] {
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			var s Stat
			var err error
			if podman {
				s, err = e.podmanSample(ctx, id)
				if err != nil && ctx.Err() == nil {
					s, err = podmanStatsError(id, err), nil
				}
			} else {
				s, err = e.sample(ctx, id)
			}
			if err != nil {
				return
			}
			mu.Lock()
			out = append(out, s)
			mu.Unlock()
		}(c.ID)
	}
	wg.Wait()
	return out, nil
}

func (e *Engine) sample(ctx context.Context, id string) (Stat, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := e.api.ContainerStats(ctx, id, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: true})
	if err != nil {
		return Stat{}, err
	}
	defer res.Body.Close()
	var v container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return Stat{}, err
	}
	return dockerStat(id, v), nil
}

// dockerStat builds a sample from one Docker stats reply that includes the previous sample.
func dockerStat(id string, v container.StatsResponse) Stat {
	s := Stat{ID: id, CPU: cpuPercent(v)}
	memStat(&s, v.MemoryStats)
	return s
}

// cpuPercent follows the docker CLI formula.
func cpuPercent(v container.StatsResponse) float64 {
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage) - float64(v.PreCPUStats.SystemUsage)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	cpus := float64(v.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(v.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	return cpuDelta / sysDelta * cpus * 100
}

// memUsed subtracts page cache like `docker stats` does (cgroup v2: inactive_file).
func memUsed(m container.MemoryStats) uint64 {
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := m.Stats[k]; ok && v < m.Usage {
			return m.Usage - v
		}
	}
	return m.Usage
}
