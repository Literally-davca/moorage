// Command moorage runs the Moorage hub, an agent, or both in one process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
	_ "time/tzdata"

	"moorage/internal/api"
	"moorage/internal/auth"
	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/host"
	"moorage/internal/monitor"
	"moorage/internal/store"
	"moorage/internal/tunnel"
)

var version = "dev"

const usage = `Moorage – Docker compose stacks, live logs and uptime in one place.

Usage:
  moorage [all-in-one]   web UI + hub + this host's Docker (default)
  moorage hub            web UI + hub only; every Docker host runs an agent
  moorage agent          connect this host's Docker to a hub (no web UI)
  moorage version        print the version

Configuration is read from environment variables, see README.md.
`

func main() {
	mode := "all-in-one"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "all-in-one", "hub", "agent":
	case "version", "--version", "-v":
		fmt.Println(version)
		return
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	setMemoryLimit()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})).With("mode", mode)
	if err := run(mode, log); err != nil {
		log.Error("moorage stopped", "err", err)
		os.Exit(1)
	}
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

func run(mode string, log *slog.Logger) error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mode == "agent" {
		return runAgent(ctx, cfg, log)
	}
	return runHub(ctx, mode, cfg, log)
}

func runAgent(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.Hub == "" {
		return errors.New("set MOORAGE_HUB to the hub's agent address, for example hub.example.com:5081")
	}
	eng, err := engine.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer eng.Close()
	eng.SyncAllHistory()
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
	a := &tunnel.Agent{
		Hub:       cfg.Hub,
		JoinToken: cfg.JoinToken,
		Dir:       filepath.Join(cfg.DataDir, "agent"),
		Handler: host.NewAgentHandler(host.Local{E: eng, ReadOnly: cfg.ReadOnly, AllowTTY: cfg.Terminal && !cfg.ReadOnly}, host.AgentOptions{
			ReadOnly: cfg.ReadOnly, Version: version, Terminal: cfg.Terminal, Prober: prober,
		}),
		Log:            log,
		WatchdogListen: cfg.WatchdogListen,
		WatchdogAddr:   cfg.WatchdogAddr,
	}
	log.Info("agent starting", "hub", cfg.Hub, "stacks", cfg.StacksDir, "readOnly", cfg.ReadOnly, "terminal", cfg.Terminal && !cfg.ReadOnly, "probes", cfg.Probes, "version", version)
	return a.Run(ctx)
}

func runHub(ctx context.Context, mode string, cfg config.Config, log *slog.Logger) error {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()

	authSvc, err := auth.New(db, log)
	if err != nil {
		return err
	}

	if err := db.CloseAbandonedTerminals(ctx, time.Now()); err != nil {
		log.Warn("close abandoned terminals", "err", err)
	}

	var local host.Host
	localName := ""
	if mode == "all-in-one" {
		if cfg.ReadOnly && cfg.Terminal {
			log.Warn("MOORAGE_TERMINAL is ignored because MOORAGE_READ_ONLY is enabled")
		}
		eng, err := engine.New(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer eng.Close()
		eng.SyncAllHistory()
		local = host.Local{E: eng, ReadOnly: cfg.ReadOnly, AllowTTY: cfg.Terminal && !cfg.ReadOnly}
		localName = eng.Host(ctx).Name
		if localName == "" {
			localName, _ = os.Hostname()
		}
	}
	reg := host.NewRegistry(local, localName, cfg.ReadOnly, db)

	monitor.UserAgent = "Moorage/" + version + " uptime check"
	mon := monitor.New(db, reg, log, cfg.PublicURL)

	var hub *tunnel.Hub
	errc := make(chan error, 3)
	if cfg.AgentListen != "" {
		ca, err := tunnel.LoadOrCreateCA(filepath.Join(cfg.DataDir, "ca"))
		if err != nil {
			return fmt.Errorf("certificate authority: %w", err)
		}
		hubName := localName
		if hubName == "" {
			hubName, _ = os.Hostname()
		}
		hub = &tunnel.Hub{CA: ca, St: db, Reg: reg, Log: log, HubName: hubName, PublicURL: cfg.PublicURL, MaintenanceUntil: mon.AllScopeUntil}
		go func() { errc <- hub.Serve(ctx, cfg.AgentListen) }()
	}

	apiSrv := api.New(cfg, authSvc, db, reg, hub, mon, log, version)
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
	mon.OnStackEvent(depSvc.StackEvent)
	if err := mon.Start(ctx); err != nil {
		return fmt.Errorf("checks: %w", err)
	}
	if err := depSvc.Start(ctx); err != nil {
		return fmt.Errorf("stack dependencies: %w", err)
	}
	apiSrv.SetHubContext(ctx)
	apiSrv.Updater().Start(ctx)
	// Remote template indexes: refreshed daily in the background (and on demand).
	apiSrv.Templates().Start(ctx)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("moorage listening", "addr", cfg.Listen, "stacks", cfg.StacksDir, "version", version, "metrics", cfg.MetricsToken != "")
		errc <- srv.ListenAndServe()
	}()

	var statusSrv *http.Server
	if cfg.StatusListen != "" {
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
			return err
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if statusSrv != nil {
		_ = statusSrv.Shutdown(shutdownCtx)
	}
	err = srv.Shutdown(shutdownCtx)
	// Shutdown doesn't wait for hijacked WebSockets; let open terminals record "hub-stopped".
	apiSrv.WaitTerminals(shutdownCtx)
	return err
}
