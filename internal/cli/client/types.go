package client

import "time"

type CoolifyNameMeta struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Type        string `json:"type,omitempty"`
	URL         string `json:"url,omitempty"`
	FQDN        string `json:"fqdn,omitempty"`
}

type HostEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Connected  bool   `json:"connected"`
	ReadOnly   bool   `json:"readOnly"`
	Version    string `json:"version,omitempty"`
	Containers int    `json:"containers,omitempty"`
	Stacks     int    `json:"stacks,omitempty"`
	LastSeen   string `json:"lastSeen,omitempty"`
}

type HostInfo struct {
	Name          string `json:"name"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion,omitempty"`
	DockerVersion string `json:"dockerVersion,omitempty"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	CPUs          int    `json:"cpus,omitempty"`
	MemBytes      int64  `json:"memBytes,omitempty"`
	StacksDir     string `json:"stacksDir,omitempty"`
}

type Container struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Service string   `json:"service,omitempty"`
	Image   string   `json:"image,omitempty"`
	State   string   `json:"state"`
	Health  string   `json:"health,omitempty"`
	Status  string   `json:"status,omitempty"`
	Ports   []string `json:"ports,omitempty"`
	URLs    []string `json:"urls,omitempty"`
}

type StackSummary struct {
	Name           string   `json:"name"`
	DisplayName    string   `json:"displayName,omitempty"`
	Status         string   `json:"status"`
	Running        int      `json:"running"`
	Total          int      `json:"total"`
	Managed        bool     `json:"managed"`
	Self           bool     `json:"self,omitempty"`
	URLs           []string `json:"urls,omitempty"`
	CoolifyUUID    string   `json:"coolifyUuid,omitempty"`
	CoolifyProject string   `json:"coolifyProject,omitempty"`
	CoolifyEnv     string   `json:"coolifyEnv,omitempty"`
	CoolifyType    string   `json:"coolifyType,omitempty"`
	CoolifyURL     string   `json:"coolifyUrl,omitempty"`
}

type Stack struct {
	StackSummary
	Location   string      `json:"location,omitempty"`
	Compose    string      `json:"compose"`
	Env        string      `json:"env"`
	Containers []Container `json:"containers"`
}

type ContainerStat struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Service    string  `json:"service,omitempty"`
	CPUPercent float64 `json:"cpuPercent"`
	MemUsage   int64   `json:"memUsage"`
	MemLimit   int64   `json:"memLimit"`
	MemPercent float64 `json:"memPercent"`
	NetRx      int64   `json:"netRx"`
	NetTx      int64   `json:"netTx"`
	BlockRead  int64   `json:"blockRead"`
	BlockWrite int64   `json:"blockWrite"`
}

type Version struct {
	Hash    string    `json:"hash"`
	Short   string    `json:"short,omitempty"`
	At      time.Time `json:"at"`
	Author  string    `json:"author"`
	Message string    `json:"message"`
	Compose string    `json:"compose,omitempty"`
	Env     string    `json:"env,omitempty"`
}

type Op struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Stack  string `json:"stack"`
	Status string `json:"status"`
}

type LintFix struct {
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Service   string   `json:"service,omitempty"`
	Line      int      `json:"line,omitempty"`
	Keys      []string `json:"keys,omitempty"`
	TargetTag string   `json:"targetTag,omitempty"`
}

type LintIssue struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Service  string   `json:"service,omitempty"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
	Fix      *LintFix `json:"fix,omitempty"`
}

type DeletedStack struct {
	Name   string    `json:"name"`
	Hash   string    `json:"hash"`
	At     time.Time `json:"at"`
	Author string    `json:"author"`
}

type TemplateEntry struct {
	Source      string `json:"source"`
	SourceLabel string `json:"sourceLabel"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Version     string `json:"version,omitempty"`
	Error       string `json:"error,omitempty"`
}

type TemplateCatalog struct {
	Templates []TemplateEntry `json:"templates"`
	LocalDir  string          `json:"localDir,omitempty"`
}

type TemplateDetail struct {
	Template  TemplateEntry `json:"template"`
	Notes     string        `json:"notes,omitempty"`
	Variables []struct {
		Name        string `json:"name"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
		Default     string `json:"default,omitempty"`
		Secret      bool   `json:"secret"`
		Required    bool   `json:"required"`
	} `json:"variables"`
	Compose   string            `json:"compose"`
	Env       string            `json:"env"`
	Warnings  []string          `json:"warnings"`
	Generated map[string]string `json:"generated,omitempty"`
}

type Check struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Target            string `json:"target"`
	TargetDisplayName string `json:"targetDisplayName,omitempty"`
	HostID            string `json:"hostId,omitempty"`
	HostName          string `json:"hostName,omitempty"`
	Stack             string `json:"stack,omitempty"`
	Interval          int    `json:"interval"`
	Enabled           bool   `json:"enabled"`
	State             struct {
		Status string `json:"status"`
		Since  string `json:"since,omitempty"`
		Last   *struct {
			At        string `json:"at"`
			Status    string `json:"status"`
			LatencyMs int    `json:"latencyMs"`
			Msg       string `json:"msg"`
		} `json:"last,omitempty"`
	} `json:"state"`
	Uptime24h *float64 `json:"uptime24h"`
}

type MaintenanceWindow struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Message   string `json:"message,omitempty"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Start     string `json:"start"`
	End       string `json:"end"`
	CreatedBy string `json:"createdBy,omitempty"`
}

type StackImages struct {
	HostID         string `json:"hostId"`
	HostName       string `json:"hostName"`
	Stack          string `json:"stack"`
	DisplayName    string `json:"displayName,omitempty"`
	CoolifyProject string `json:"coolifyProject,omitempty"`
	Status         string `json:"status"`
	Policy         struct {
		Mode string `json:"mode"`
	} `json:"policy"`
	Services []struct {
		Service         string `json:"service"`
		Image           string `json:"image"`
		CurrentTag      string `json:"currentTag"`
		LatestTag       string `json:"latestTag,omitempty"`
		DigestChanged   bool   `json:"digestChanged"`
		UpdateAvailable bool   `json:"updateAvailable"`
	} `json:"services"`
}

type DepNode struct {
	HostID      string `json:"hostId"`
	HostName    string `json:"hostName,omitempty"`
	Stack       string `json:"stack"`
	DisplayName string `json:"displayName,omitempty"`
	Status      string `json:"status,omitempty"`
	External    bool   `json:"external,omitempty"`
	InCycle     bool   `json:"inCycle,omitempty"`
}

type StackDep struct {
	ID          int64   `json:"id"`
	From        DepNode `json:"from"`
	To          DepNode `json:"to"`
	Source      string  `json:"source"`
	Detail      string  `json:"detail,omitempty"`
	Ignored     bool    `json:"ignored"`
	AutoRestart string  `json:"autoRestart,omitempty"`
}

type HostDeps struct {
	Edges []StackDep `json:"edges"`
	Nodes []DepNode  `json:"nodes"`
}

type ImportPreview struct {
	Token      string `json:"token"`
	ExportedAt string `json:"exportedAt"`
	Version    string `json:"version"`
	Database   struct {
		SchemaVersion int `json:"schemaVersion"`
		Users         int `json:"users"`
		Agents        int `json:"agents"`
		Checks        int `json:"checks"`
		Channels      int `json:"channels"`
		StatusPages   int `json:"statusPages"`
	} `json:"database"`
	Hosts []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Known    bool   `json:"known"`
		Writable bool   `json:"writable"`
		Stacks   []struct {
			Name   string `json:"name"`
			Exists bool   `json:"exists"`
		} `json:"stacks"`
	} `json:"hosts"`
	Skipped []string `json:"skipped,omitempty"`
}

type SystemUpdatesOverview struct {
	Channel         string `json:"channel"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable,omitempty"`
	CheckError      string `json:"checkError,omitempty"`
	Nodes           []struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Kind            string `json:"kind"`
		CurrentVersion  string `json:"currentVersion"`
		TargetVersion   string `json:"targetVersion,omitempty"`
		CanRollback     bool   `json:"canRollback"`
		RollbackVersion string `json:"rollbackVersion,omitempty"`
		Job             *struct {
			ID      int64  `json:"id"`
			Status  string `json:"status"`
			Message string `json:"message,omitempty"`
		} `json:"job,omitempty"`
	} `json:"nodes"`
}

type AuditEntry struct {
	ID               int64  `json:"id"`
	At               string `json:"at"`
	Actor            string `json:"actor"`
	IP               string `json:"ip"`
	Action           string `json:"action"`
	HostID           string `json:"hostId"`
	HostName         string `json:"hostName,omitempty"`
	Stack            string `json:"stack"`
	StackDisplayName string `json:"stackDisplayName,omitempty"`
	Target           string `json:"target"`
	Outcome          string `json:"outcome"`
}

type APIToken struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"`
	Role       string    `json:"role"`
	Scopes     []string  `json:"scopes"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

type LogLine struct {
	Time      string `json:"t,omitempty"`
	Container string `json:"c,omitempty"`
	Service   string `json:"svc,omitempty"`
	Project   string `json:"p,omitempty"`
	Stream    string `json:"s,omitempty"`
	Msg       string `json:"m"`
}

type PrunePreview struct {
	Containers  int   `json:"containers"`
	Images      int   `json:"images"`
	Networks    int   `json:"networks"`
	BuildCache  int64 `json:"buildCache"`
	Volumes     int   `json:"volumes"`
	Reclaimable int64 `json:"reclaimableBytes"`
}
