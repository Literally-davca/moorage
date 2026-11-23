// Package convert turns a `docker run …` command into a compose.yaml service.
package convert

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Result is the generated compose file plus notes about options that were not converted.
type Result struct {
	YAML     string   `json:"yaml"`
	Service  string   `json:"service"`
	Warnings []string `json:"warnings"`
}

type service struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name,omitempty"`
	Hostname      string            `yaml:"hostname,omitempty"`
	Restart       string            `yaml:"restart,omitempty"`
	Entrypoint    []string          `yaml:"entrypoint,omitempty,flow"`
	Command       []string          `yaml:"command,omitempty,flow"`
	WorkingDir    string            `yaml:"working_dir,omitempty"`
	User          string            `yaml:"user,omitempty"`
	Ports         []string          `yaml:"ports,omitempty"`
	Expose        []string          `yaml:"expose,omitempty"`
	Volumes       []string          `yaml:"volumes,omitempty"`
	Tmpfs         []string          `yaml:"tmpfs,omitempty"`
	Environment   map[string]string `yaml:"environment,omitempty"`
	EnvFile       []string          `yaml:"env_file,omitempty"`
	Labels        map[string]string `yaml:"labels,omitempty"`
	NetworkMode   string            `yaml:"network_mode,omitempty"`
	Networks      []string          `yaml:"networks,omitempty"`
	DNS           []string          `yaml:"dns,omitempty"`
	ExtraHosts    []string          `yaml:"extra_hosts,omitempty"`
	CapAdd        []string          `yaml:"cap_add,omitempty"`
	CapDrop       []string          `yaml:"cap_drop,omitempty"`
	Devices       []string          `yaml:"devices,omitempty"`
	SecurityOpt   []string          `yaml:"security_opt,omitempty"`
	Sysctls       map[string]string `yaml:"sysctls,omitempty"`
	Privileged    bool              `yaml:"privileged,omitempty"`
	Init          bool              `yaml:"init,omitempty"`
	Tty           bool              `yaml:"tty,omitempty"`
	StdinOpen     bool              `yaml:"stdin_open,omitempty"`
	ShmSize       string            `yaml:"shm_size,omitempty"`
	MemLimit      string            `yaml:"mem_limit,omitempty"`
	Cpus          string            `yaml:"cpus,omitempty"`
	Platform      string            `yaml:"platform,omitempty"`
	PullPolicy    string            `yaml:"pull_policy,omitempty"`
	StopSignal    string            `yaml:"stop_signal,omitempty"`
	Ipc           string            `yaml:"ipc,omitempty"`
	Pid           string            `yaml:"pid,omitempty"`
	Logging       *logging          `yaml:"logging,omitempty"`
}

type logging struct {
	Driver  string            `yaml:"driver,omitempty"`
	Options map[string]string `yaml:"options,omitempty"`
}

type external struct {
	External bool `yaml:"external"`
}

type file struct {
	Services map[string]*service  `yaml:"services"`
	Networks map[string]external  `yaml:"networks,omitempty"`
	Volumes  map[string]yaml.Node `yaml:"volumes,omitempty"`
}

// flags that take a value; the value is the next argument or follows "=".
var valueFlags = map[string]string{
	"--name": "name", "-p": "publish", "--publish": "publish", "-v": "volume", "--volume": "volume",
	"-e": "env", "--env": "env", "--env-file": "env-file", "-l": "label", "--label": "label",
	"--restart": "restart", "--network": "network", "--net": "network", "-h": "hostname", "--hostname": "hostname",
	"-w": "workdir", "--workdir": "workdir", "-u": "user", "--user": "user", "--entrypoint": "entrypoint",
	"--cap-add": "cap-add", "--cap-drop": "cap-drop", "--device": "device", "--dns": "dns", "--add-host": "add-host",
	"--shm-size": "shm-size", "-m": "memory", "--memory": "memory", "--cpus": "cpus", "--log-driver": "log-driver",
	"--log-opt": "log-opt", "--pull": "pull", "--security-opt": "security-opt", "--sysctl": "sysctl",
	"--tmpfs": "tmpfs", "--platform": "platform", "--stop-signal": "stop-signal", "--expose": "expose",
	"--ipc": "ipc", "--pid": "pid", "--mount": "mount", "--gpus": "gpus", "--ulimit": "ulimit",
	"--health-cmd": "health", "--health-interval": "health", "--health-retries": "health", "--health-timeout": "health",
	"--network-alias": "unsupported", "--ip": "unsupported", "--memory-swap": "unsupported", "--cpuset-cpus": "unsupported",
	"--group-add": "unsupported", "--link": "unsupported", "--volumes-from": "unsupported", "--cidfile": "unsupported",
	"-a": "unsupported", "--attach": "unsupported",
}

var boolFlags = map[string]string{
	"-d": "detach", "--detach": "detach", "--rm": "rm", "-i": "interactive", "--interactive": "interactive",
	"-t": "tty", "--tty": "tty", "--privileged": "privileged", "--init": "init", "-P": "publish-all",
	"--publish-all": "publish-all", "--read-only": "unsupported", "--no-healthcheck": "unsupported",
}

var nameSanitize = regexp.MustCompile(`[^a-z0-9_-]+`)

// DockerRun converts one docker run command.
func DockerRun(cmd string) (Result, error) {
	args, err := SplitWords(cmd)
	if err != nil {
		return Result{}, err
	}
	switch {
	case len(args) >= 2 && args[0] == "docker" && args[1] == "run":
		args = args[2:]
	case len(args) >= 3 && args[0] == "docker" && args[1] == "container" && args[2] == "run":
		args = args[3:]
	case len(args) >= 1 && args[0] == "run":
		args = args[1:]
	default:
		return Result{}, errors.New("paste a command that starts with `docker run`")
	}

	s := &service{}
	var warns []string
	warn := func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }
	namedVolumes := map[string]bool{}
	var networks []string

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break // image
		}
		flag, val, hasVal := a, "", false
		if strings.HasPrefix(a, "--") {
			flag, val, hasVal = strings.Cut(a, "=")
		} else if len(a) > 2 {
			// -p8080:80 or combined booleans like -dit
			if _, ok := valueFlags[a[:2]]; ok {
				flag, val, hasVal = a[:2], strings.TrimPrefix(a[2:], "="), true
			} else {
				ok := true
				for _, r := range a[1:] {
					if _, b := boolFlags["-"+string(r)]; !b {
						ok = false
					}
				}
				if !ok {
					warn("Skipped unknown option %s.", a)
					continue
				}
				for _, r := range a[1:] {
					applyBool(s, boolFlags["-"+string(r)], "-"+string(r), warn)
				}
				continue
			}
		}

		if kind, ok := boolFlags[flag]; ok {
			if hasVal && val == "false" {
				continue
			}
			applyBool(s, kind, flag, warn)
			continue
		}
		kind, ok := valueFlags[flag]
		if !ok {
			warn("Skipped unknown option %s.", flag)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return Result{}, fmt.Errorf("option %s needs a value", flag)
			}
			i++
			val = args[i]
		}
		switch kind {
		case "name":
			s.ContainerName = val
		case "publish":
			s.Ports = append(s.Ports, val)
		case "volume":
			s.Volumes = append(s.Volumes, val)
			src, _, _ := strings.Cut(val, ":")
			if src != "" && !strings.ContainsAny(src, "/~.$") {
				namedVolumes[src] = true
			}
		case "env":
			if s.Environment == nil {
				s.Environment = map[string]string{}
			}
			k, v, _ := strings.Cut(val, "=")
			s.Environment[k] = v
		case "env-file":
			s.EnvFile = append(s.EnvFile, val)
		case "label":
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			k, v, _ := strings.Cut(val, "=")
			s.Labels[k] = v
		case "restart":
			s.Restart = val
		case "network":
			switch val {
			case "host", "bridge", "none":
				s.NetworkMode = val
			default:
				if strings.HasPrefix(val, "container:") || strings.HasPrefix(val, "service:") {
					s.NetworkMode = val
				} else {
					networks = append(networks, val)
				}
			}
		case "hostname":
			s.Hostname = val
		case "workdir":
			s.WorkingDir = val
		case "user":
			s.User = val
		case "entrypoint":
			s.Entrypoint = []string{val}
		case "cap-add":
			s.CapAdd = append(s.CapAdd, val)
		case "cap-drop":
			s.CapDrop = append(s.CapDrop, val)
		case "device":
			s.Devices = append(s.Devices, val)
		case "dns":
			s.DNS = append(s.DNS, val)
		case "add-host":
			s.ExtraHosts = append(s.ExtraHosts, val)
		case "shm-size":
			s.ShmSize = val
		case "memory":
			s.MemLimit = val
		case "cpus":
			s.Cpus = val
		case "log-driver":
			if s.Logging == nil {
				s.Logging = &logging{}
			}
			s.Logging.Driver = val
		case "log-opt":
			if s.Logging == nil {
				s.Logging = &logging{}
			}
			if s.Logging.Options == nil {
				s.Logging.Options = map[string]string{}
			}
			k, v, _ := strings.Cut(val, "=")
			s.Logging.Options[k] = v
		case "pull":
			s.PullPolicy = val
		case "security-opt":
			s.SecurityOpt = append(s.SecurityOpt, val)
		case "sysctl":
			if s.Sysctls == nil {
				s.Sysctls = map[string]string{}
			}
			k, v, _ := strings.Cut(val, "=")
			s.Sysctls[k] = v
		case "tmpfs":
			s.Tmpfs = append(s.Tmpfs, val)
		case "platform":
			s.Platform = val
		case "stop-signal":
			s.StopSignal = val
		case "expose":
			s.Expose = append(s.Expose, val)
		case "ipc":
			s.Ipc = val
		case "pid":
			s.Pid = val
		case "mount":
			warn("--mount %s isn't converted. Add it under volumes: using the long syntax.", val)
		case "gpus":
			warn("--gpus %s isn't converted. Use deploy.resources.reservations.devices.", val)
		case "ulimit":
			warn("--ulimit %s isn't converted. Use ulimits:.", val)
		case "health":
			warn("%s isn't converted. Use healthcheck:.", flag)
		default:
			warn("%s isn't supported in compose and was skipped.", flag)
		}
	}

	if i >= len(args) {
		return Result{}, errors.New("the command has no image")
	}
	s.Image = args[i]
	if rest := args[i+1:]; len(rest) > 0 {
		s.Command = rest
	}

	name := serviceName(s)
	f := file{Services: map[string]*service{name: s}}
	if len(networks) > 0 {
		s.Networks = networks
		f.Networks = map[string]external{}
		for _, n := range networks {
			f.Networks[n] = external{External: true}
		}
		warn("Networks %s are marked external: create them first, or remove external: true.", strings.Join(networks, ", "))
	}
	if len(namedVolumes) > 0 {
		f.Volumes = map[string]yaml.Node{}
		for v := range namedVolumes {
			f.Volumes[v] = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
		}
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // compose files conventionally use two spaces
	if err := enc.Encode(f); err != nil {
		return Result{}, err
	}
	if err := enc.Close(); err != nil {
		return Result{}, err
	}
	if warns == nil {
		warns = []string{}
	}
	return Result{YAML: buf.String(), Service: name, Warnings: warns}, nil
}

func applyBool(s *service, kind, flag string, warn func(string, ...any)) {
	switch kind {
	case "detach", "rm":
		// compose services are detached by default; --rm has no compose meaning
	case "interactive":
		s.StdinOpen = true
	case "tty":
		s.Tty = true
	case "privileged":
		s.Privileged = true
	case "init":
		s.Init = true
	case "publish-all":
		warn("-P (publish all) isn't converted. List the ports under ports:.")
	default:
		warn("%s isn't supported in compose and was skipped.", flag)
	}
}

func serviceName(s *service) string {
	base := s.ContainerName
	if base == "" {
		img := s.Image
		if i := strings.LastIndex(img, "@"); i > 0 {
			img = img[:i]
		}
		img = path.Base(img)
		if i := strings.LastIndex(img, ":"); i > 0 {
			img = img[:i]
		}
		base = img
	}
	n := strings.Trim(nameSanitize.ReplaceAllString(strings.ToLower(base), "-"), "-_")
	if n == "" {
		n = "app"
	}
	return n
}

// SplitWords splits a shell command line: quotes, backslash escapes and line continuations.
func SplitWords(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		inTok bool
		quote rune
	)
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`\n", rs[i+1]):
				i++
				if rs[i] != '\n' {
					cur.WriteRune(rs[i])
				}
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			if i+1 < len(rs) {
				i++
				if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
					i++
					continue
				}
				if rs[i] != '\n' {
					cur.WriteRune(rs[i])
					inTok = true
				}
			}
		case r == '\'' || r == '"':
			quote = r
			inTok = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inTok {
				words = append(words, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(r)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if inTok {
		words = append(words, cur.String())
	}
	return words, nil
}
