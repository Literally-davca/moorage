// Command moorage runs the Moorage hub, an agent, or both in one process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/moby/moby/client"

	"moorage/internal/api"
	"moorage/internal/auth"
	clicmd "moorage/internal/cli/cmd"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/selfupdate"
	"moorage/internal/store"
	"moorage/internal/tunnel"
)

var version = "dev"

func main() {
	if len(os.Args) <= 1 {
		if err := runServer("all-in-one"); err != nil {
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "self-update-helper":
		if err := runSelfUpdateHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "self-update-helper: %v\n", err)
			os.Exit(1)
		}
		return
	case "self-stack-helper":
		if err := runSelfStackHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "self-stack-helper: %v\n", err)
			os.Exit(1)
		}
		return
	case "self-update-watch":
		if err := runSelfUpdateWatch(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "self-update-watch: %v\n", err)
			os.Exit(1)
		}
		return
	case "host-shell-helper":
		if err := runHostShellHelper(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "host-shell-helper: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := clicmd.Execute(version, os.Args[1:], clicmd.ServerHooks{RunServer: runServer}); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func runServer(mode string) error {
	setMemoryLimit()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("mode", mode)
	reexec, err := run(mode, log)
	if err != nil {
		log.Error("moorage stopped", "err", err)
		return err
	}
	if reexec != nil {
		if err := reexec(); err != nil {
			log.Error("moorage re-exec failed", "err", err)
			return err
		}
	}
	return nil
}

func runSelfUpdateHelper(args []string) error {
	opt, err := selfupdate.ParseHelperFlags(args)
	if err != nil {
		return err
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()
	return selfupdate.RunHelper(context.Background(), selfupdate.MobyEngine{API: cli}, opt)
}

// runSelfStackHelper runs Apply & restart of the stack Moorage runs from, from a
// one-shot container: compose up, wait for Moorage, roll back the files if it fails.
func runSelfStackHelper(args []string) error {
	opt, err := selfupdate.ParseStackApplyFlags(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("mode", "self-stack-helper")
	cfg := config.Config{DataDir: opt.DataDir, StacksDir: filepath.Dir(opt.StackDir)}
	eng, err := engine.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer eng.Close()
	up := func(c context.Context, out io.Writer) error {
		return eng.ComposeUpDir(c, opt.Project, opt.StackDir, out)
	}
	return selfupdate.RunStackApplyHelper(ctx, selfupdate.MobyEngine{API: eng.RawClient()}, up, opt)
}

// wireSelfStack finds the compose stack this node runs from and lets the engine refuse
// stopping it and apply changes to it through the helper container.
func wireSelfStack(ctx context.Context, eng *engine.Engine, cfg config.Config, isHub bool, log *slog.Logger) {
	wireRuntimeNet(ctx, eng, cfg, log)
	me := selfupdate.MobyEngine{API: eng.RawClient()}
	s, insp := selfupdate.DetectSelfStack(ctx, me, cfg.DataDir)
	if s == nil {
		return
	}
	s.Launch = func(c context.Context) error {
		return selfupdate.LaunchStackApplyHelper(c, me, insp, selfupdate.StackApplyLaunch{
			Self: s, StacksDir: cfg.StacksDir, DataDir: cfg.DataDir, IsHub: isHub,
			ListenAddr: cfg.Listen, Version: version, DockerHost: os.Getenv("DOCKER_HOST"),
		})
	}
	eng.SetSelf(s)
	log.Info("Moorage runs from a stack here", "stack", s.Project, "service", s.Service)
}

func runSelfUpdateWatch(args []string) error {
	var statePath string
	var detach bool
	fs := flag.NewFlagSet("self-update-watch", flag.ContinueOnError)
	fs.StringVar(&statePath, "state", "", "pending state JSON file")
	fs.BoolVar(&detach, "detach", false, "start the watcher in the background and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if statePath == "" {
		return errors.New("--state is required")
	}
	if detach {
		// Intermediate of the double fork: start the real watcher and exit at once, so the
		// watcher is reparented to init, which reaps it.
		return selfupdate.SpawnWatcherChild(statePath)
	}
	return selfupdate.RunWatch(statePath, nil)
}

// defaultMemoryLimit is a soft limit for the Go runtime. Go collects garbage when the
// heap doubles since the last collection, so the garbage made while starting up kept
// the hub at ~78 MiB anonymous memory (107 MiB RSS) for its first two minutes, until
// the forced GC brought it down to ~19 MiB. With the limit the GC runs earlier and the
// hub stays at ~17 MiB from the start. It is soft: a bigger live heap only means more
// frequent collections, never an out-of-memory error. GOMEMLIMIT overrides it.
const defaultMemoryLimit = 48 << 20

func setMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemoryLimit)
	}
}

func run(mode string, log *slog.Logger) (func() error, error) {
	cfg, err := config.FromEnv()
	if err != nil {
		return nil, err
	}
	// A freshly installed binary that keeps failing to come up (crash loop under systemd,
	// where the watcher dies with the service) restores moorage.prev and execs it here.
	startedAt := time.Now()
	if restored := selfupdate.BootCheck(cfg.DataDir, version, startedAt, log); restored != "" {
		return func() error { return selfupdate.ExecRestored(restored) }, nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var reexec func() error
	if mode == "agent" {
		reexec, err = runAgent(ctx, cfg, log)
	} else {
		reexec, err = runHub(ctx, mode, cfg, log)
	}
	if err == nil && reexec == nil && selfupdate.RolledBackAway(cfg.DataDir, version, startedAt) {
		// The watcher restored the previous binary and stopped us: exit non-zero so
		// systemd's Restart=on-failure starts the restored version.
		err = errors.New("stopped by an automatic self-update rollback")
	}
	return reexec, err
}

func runAgent(parentCtx context.Context, cfg config.Config, log *slog.Logger) (func() error, error) {
	if cfg.Hub == "" {
		return nil, errors.New("set MOORAGE_HUB to the hub's agent address, for example hub.example.com:5081")
	}
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	eng, err := engine.New(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	defer eng.Close()
	eng.SyncAllHistory()
	wireSelfStack(ctx, eng, cfg, false, log)
	if cfg.ReadOnly && cfg.Terminal {
		log.Warn("MOORAGE_TERMINAL is ignored because MOORAGE_READ_ONLY is enabled")
	}
	if cfg.MetricsToken != "" {
		log.Warn("MOORAGE_METRICS_TOKEN is ignored on an agent; the hub serves /metrics for every host")
	}
	// Remote probes for multi-location checks. A read-only agent probes too: a probe only
	// reads the network and changes nothing on this host.
	var prober host.Prober
	if cfg.Probes {
		monitor.UserAgent = "Moorage/" + version + " uptime check"
		prober = monitor.NewAgentProber()
	}
	nodeUpd := &selfupdate.NodeUpdater{
		DataDir:        cfg.DataDir,
		CurrentVersion: version,
		SelfUpdate:     cfg.SelfUpdate,
		IsHub:          false,
		Eng:            selfupdate.MobyEngine{API: eng.RawClient()},
		ShutdownFn:     cancel,
		Log:            log,
	}
	nodeUpd.StartCleanupLoop(ctx)

	a := &tunnel.Agent{
		Hub:       cfg.Hub,
		JoinToken: cfg.JoinToken,
		Dir:       filepath.Join(cfg.DataDir, "agent"),
		Handler: host.NewAgentHandler(host.Local{E: eng, ReadOnly: cfg.ReadOnly, AllowTTY: cfg.Terminal && !cfg.ReadOnly}, host.AgentOptions{
			ReadOnly: cfg.ReadOnly, Version: version, Terminal: cfg.Terminal, Prober: prober, SelfUpdate: nodeUpd,
		}),
		Log:            log,
		WatchdogListen: cfg.WatchdogListen,
		WatchdogAddr:   cfg.WatchdogAddr,
	}
	log.Info("agent starting", "hub", cfg.Hub, "stacks", cfg.StacksDir, "readOnly", cfg.ReadOnly, "terminal", cfg.Terminal && !cfg.ReadOnly, "probes", cfg.Probes, "selfUpdate", cfg.SelfUpdate, "version", version)
	if err := a.Run(ctx); err != nil {
		return nil, err
	}
	if nodeUpd.PendingExec() != "" {
		return nodeUpd.ExecPending, nil
	}
	return nil, nil
}

func runHub(parentCtx context.Context, mode string, cfg config.Config, log *slog.Logger) (func() error, error) {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Settings saved in the UI fill in what the env leaves unset. The public URL feeds
	// alert links, OIDC redirects, invites and status pages, so it is read once here and a
	// change takes effect after a restart; the update channel also switches live.
	if !cfg.PublicURLSet {
		if v, err := db.GetSetting(ctx, store.SettingPublicURL); err == nil && v != "" {
			cfg.PublicURL = v
		}
	}
	if !cfg.UpdateChannelSet {
		if v, err := db.GetSetting(ctx, store.SettingUpdateChannel); err == nil && (v == "stable" || v == "pre") {
			cfg.UpdateChannel = v
		}
	}

	authSvc, err := auth.New(db, log)
	if err != nil {
		return nil, err
	}

	if err := db.CloseAbandonedTerminals(ctx, time.Now()); err != nil {
		log.Warn("close abandoned terminals", "err", err)
	}

	var local host.Host
	var selfEng selfupdate.ContainerEngine
	localName := ""
	if mode == "all-in-one" {
		if cfg.ReadOnly && cfg.Terminal {
			log.Warn("MOORAGE_TERMINAL is ignored because MOORAGE_READ_ONLY is enabled")
		}
		eng, err := engine.New(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		defer eng.Close()
		eng.SyncAllHistory()
		wireSelfStack(ctx, eng, cfg, true, log)
		selfEng = selfupdate.MobyEngine{API: eng.RawClient()}
		local = host.Local{E: eng, ReadOnly: cfg.ReadOnly, AllowTTY: cfg.Terminal && !cfg.ReadOnly}
		localName = eng.Host(ctx).Name
		if localName == "" {
			localName, _ = os.Hostname()
		}
	} else if cli, ok := optionalHubDockerClient(); ok {
		defer cli.Close()
		selfEng = selfupdate.MobyEngine{API: cli}
	}
	reg := host.NewRegistry(local, localName, cfg.ReadOnly, db)

	monitor.UserAgent = "Moorage/" + version + " uptime check"
	mon := monitor.New(db, reg, log, cfg.PublicURL)

	nodeUpd := &selfupdate.NodeUpdater{
		DataDir:        cfg.DataDir,
		CurrentVersion: version,
		SelfUpdate:     cfg.SelfUpdate,
		IsHub:          true,
		ListenAddr:     cfg.Listen,
		Eng:            selfEng,
		ShutdownFn:     cancel,
		Log:            log,
	}

	var hub *tunnel.Hub
	errc := make(chan error, 4)
	if cfg.AgentListen != "" {
		ca, err := tunnel.LoadOrCreateCA(filepath.Join(cfg.DataDir, "ca"))
		if err != nil {
			return nil, fmt.Errorf("certificate authority: %w", err)
		}
		hubName := localName
		if hubName == "" {
			hubName, _ = os.Hostname()
		}
		hub = &tunnel.Hub{CA: ca, St: db, Reg: reg, Log: log, HubName: hubName, PublicURL: cfg.PublicURL, MaintenanceUntil: mon.AllScopeUntil}
	}

	apiSrv := api.New(cfg, authSvc, db, reg, hub, mon, log, version)
	sysUpd := selfupdate.NewService(cfg, version, mode == "all-in-one", db, reg, mon, hub, nodeUpd, func(c context.Context, e store.AuditEntry) {
		if err := db.Audit(c, e); err != nil && log != nil {
			log.Warn("audit self-update", "err", err)
		}
	}, log)
	apiSrv.SetSelfUpdate(sysUpd)
	if hub != nil {
		hub.OnConnect = func(id string) {
			sysUpd.OnAgentConnect(id)
			go apiSrv.SyncRegistriesToHost(id)
		}
		hub.MaintenanceUntil = func(now time.Time) time.Time {
			u := mon.AllScopeUntil(now)
			if hu := sysUpd.HubUpdateUntil(now); hu.After(u) {
				return hu
			}
			return u
		}
		go func() { errc <- hub.Serve(ctx, cfg.AgentListen) }()
	}

	// A window starting or ending changes status pages and, for "everything" windows,
	// the pause of the agents' hub watchdog.
	mon.OnMaintenanceChange(func() {
		apiSrv.StatusService().Invalidate()
		if hub != nil {
			go hub.PushWatchdog(ctx)
		}
	})
	// Stack dependencies: cascade grouping of alerts, and re-detection on saves and deploys.
	depSvc := apiSrv.Deps()
	mon.SetDeps(depSvc)
	mon.OnStackEvent(apiSrv.StackEventHook)
	if err := mon.Start(ctx); err != nil {
		return nil, fmt.Errorf("checks: %w", err)
	}
	if err := depSvc.Start(ctx); err != nil {
		return nil, fmt.Errorf("stack dependencies: %w", err)
	}
	apiSrv.StartAutoRestart(ctx)
	apiSrv.StartCoolify(ctx)
	apiSrv.StartScheduler(ctx)
	apiSrv.SetHubContext(ctx)
	apiSrv.SyncRegistries(ctx)
	apiSrv.StartCleanupLoop(ctx)
	// OIDC discovery runs in the background and retries: an IdP that is down or a broken
	// setting never stops Moorage (the Users page shows the problem to admins).
	if cfg.WebUI {
		apiSrv.StartSSO(ctx)
	}
	apiSrv.Updater().Start(ctx)
	// Remote template indexes: refreshed daily in the background (and on demand).
	apiSrv.Templates().Start(ctx)
	sysUpd.Start(ctx)

	var sockSrv *http.Server
	var sockLn net.Listener
	if cfg.SocketPath != "" {
		var sockCleanup func()
		sockLn, sockCleanup, err = listenLocalSocket(cfg.SocketPath)
		if err != nil {
			return nil, err
		}
		defer sockCleanup()
		if sockLn != nil {
			sockSrv = &http.Server{
				Handler:           apiSrv.SocketHandler(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			go func() {
				log.Info("local CLI socket listening", "socket", cfg.SocketPath)
				errc <- sockSrv.Serve(sockLn)
			}()
		}
	}

	var srv *http.Server
	if cfg.WebUI && cfg.Listen != "" {
		srv = &http.Server{
			Addr:              cfg.Listen,
			Handler:           apiSrv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
		}
		go func() {
			log.Info("moorage listening", "addr", cfg.Listen, "stacks", cfg.StacksDir, "version", version, "metrics", cfg.MetricsToken != "", "webUI", true)
			errc <- srv.Serve(ln)
		}()
	} else {
		if sockLn == nil {
			return nil, fmt.Errorf("web UI is disabled (MOORAGE_WEB_UI=false / no MOORAGE_LISTEN) and no local Unix socket is available")
		}
		log.Info("moorage listening in CLI-only mode (web UI disabled)", "socket", cfg.SocketPath, "stacks", cfg.StacksDir, "version", version)
	}
	// A hub update is confirmed only once this process actually starts serving.
	sysUpd.MarkServing()

	var statusSrv *http.Server
	if cfg.WebUI && cfg.StatusListen != "" {
		statusSrv = &http.Server{
			Addr:              cfg.StatusListen,
			Handler:           apiSrv.StatusHandler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info("status page listener starting", "addr", cfg.StatusListen)
			errc <- statusSrv.ListenAndServe()
		}()
	}

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return nil, err
		}
	case <-ctx.Done():
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if statusSrv != nil {
		_ = statusSrv.Shutdown(shutdownCtx)
	}
	if sockSrv != nil {
		_ = sockSrv.Shutdown(shutdownCtx)
	}
	if srv != nil {
		err = srv.Shutdown(shutdownCtx)
	} else {
		err = nil
	}
	// Shutdown doesn't wait for hijacked WebSockets; let open terminals record "hub-stopped".
	apiSrv.WaitTerminals(shutdownCtx)
	if err != nil {
		return nil, err
	}
	if nodeUpd.PendingExec() != "" {
		return nodeUpd.ExecPending, nil
	}
	return nil, nil
}

func optionalHubDockerClient() (*client.Client, bool) {
	if os.Getenv("DOCKER_HOST") == "" {
		if _, err := os.Stat("/var/run/docker.sock"); err != nil {
			return nil, false
		}
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, false
	}
	return cli, true
}
