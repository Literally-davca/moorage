package main

import (
	"context"
	"log/slog"

	"moorage/internal/config"
	"moorage/internal/engine"
	"moorage/internal/selfupdate"
)

// wireRuntimeNet tells the engine how this node is attached to the host network
// (native / host network / bridge) and where its data directory lives on the host (M17B:
// port map, host addresses, add-on defaults).
func wireRuntimeNet(ctx context.Context, eng *engine.Engine, cfg config.Config, log *slog.Logger) {
	rt := selfupdate.DetectRuntimeNet(ctx, selfupdate.MobyEngine{API: eng.RawClient()}, cfg.DataDir)
	eng.SetRuntimeNet(rt)
	log.Debug("host network", "mode", rt.Mode, "dataHostPath", rt.DataHostPath)
}
