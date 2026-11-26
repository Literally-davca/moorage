package selfupdate

import (
	"context"
	"net"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"moorage/internal/engine"
)

// DetectRuntimeNet says how this Moorage process is attached to the host network (M17B):
// a binary on the host (native), a container with network_mode: host, or a container on
// its own network (bridge). It also finds the host path of the data directory when that
// is a bind mount, so add-ons can put backups next to Moorage's data.
func DetectRuntimeNet(ctx context.Context, eng ContainerEngine, dataDir string) engine.RuntimeNet {
	re := defaultRuntimeEnv(true)
	if !inContainerEnv(re) {
		return engine.RuntimeNet{Mode: engine.NetModeNative, DataHostPath: filepath.Clean(dataDir)}
	}
	var insp *container.InspectResponse
	if eng != nil {
		insp, _ = findSelfContainer(ctx, eng, re)
	}
	return runtimeNetFromInspect(insp, dataDir, hasHostInterfaces())
}

func runtimeNetFromInspect(insp *container.InspectResponse, dataDir string, hostIfaces bool) engine.RuntimeNet {
	rt := engine.RuntimeNet{Mode: engine.NetModeBridge}
	if insp == nil {
		// No engine access to our own container: a container that sees docker0 or a
		// physical interface shares the host's network namespace.
		if hostIfaces {
			rt.Mode = engine.NetModeHost
		}
		return rt
	}
	if insp.HostConfig != nil && insp.HostConfig.NetworkMode == "host" {
		rt.Mode = engine.NetModeHost
	}
	rt.DataHostPath = bindSourceOf(insp, dataDir)
	return rt
}

// bindSourceOf maps dir inside the container to its host path when the mount holding it
// is a bind mount.
func bindSourceOf(insp *container.InspectResponse, dir string) string {
	clean := filepath.ToSlash(filepath.Clean(dir))
	var best container.MountPoint
	found := false
	for _, m := range insp.Mounts {
		d := filepath.ToSlash(filepath.Clean(m.Destination))
		if (clean == d || strings.HasPrefix(clean, d+"/")) && len(d) > len(best.Destination) {
			best, found = m, true
		}
	}
	if !found || best.Type != mount.TypeBind || best.Source == "" {
		return ""
	}
	rel := strings.TrimPrefix(clean, filepath.ToSlash(filepath.Clean(best.Destination)))
	return strings.TrimRight(filepath.ToSlash(best.Source), "/") + rel
}

// hasHostInterfaces reports whether this network namespace has a Docker bridge or a
// physical / VPN interface, which a container on its own network never sees.
func hasHostInterfaces() bool {
	ifs, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifs {
		n := ifc.Name
		if n == "docker0" || strings.HasPrefix(n, "wl") || strings.HasPrefix(n, "tailscale") || strings.HasPrefix(n, "en") {
			return true
		}
	}
	return false
}
