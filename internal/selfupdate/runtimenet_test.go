package selfupdate

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"moorage/internal/engine"
)

func TestRuntimeNetFromInspect(t *testing.T) {
	insp := &container.InspectResponse{
		HostConfig: &container.HostConfig{NetworkMode: "host"},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/srv/moorage", Destination: "/data"},
			{Type: mount.TypeVolume, Source: "/var/lib/docker/volumes/x/_data", Destination: "/data/cache"},
		},
	}
	rt := runtimeNetFromInspect(insp, "/data", false)
	if rt.Mode != engine.NetModeHost || rt.DataHostPath != "/srv/moorage" {
		t.Fatalf("got %+v", rt)
	}
	if got := bindSourceOf(insp, "/data/backups"); got != "/srv/moorage/backups" {
		t.Fatalf("nested path: %q", got)
	}
	if got := bindSourceOf(insp, "/data/cache"); got != "" {
		t.Fatalf("named volume must not map to a host path: %q", got)
	}

	bridge := &container.InspectResponse{HostConfig: &container.HostConfig{NetworkMode: "moorage_default"},
		Mounts: []container.MountPoint{{Type: mount.TypeVolume, Destination: "/data"}}}
	if rt := runtimeNetFromInspect(bridge, "/data", true); rt.Mode != engine.NetModeBridge || rt.DataHostPath != "" {
		t.Fatalf("bridge: %+v", rt)
	}
	if rt := runtimeNetFromInspect(nil, "/data", true); rt.Mode != engine.NetModeHost {
		t.Fatalf("no inspect, host interfaces visible: %+v", rt)
	}
	if rt := runtimeNetFromInspect(nil, "/data", false); rt.Mode != engine.NetModeBridge {
		t.Fatalf("no inspect: %+v", rt)
	}
}
