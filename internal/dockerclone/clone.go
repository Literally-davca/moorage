// Package dockerclone copies a container's runtime configuration onto a new image, for
// swaps that must keep everything else (self-update, rolling deploys, recreating a
// standalone container). It depends on the Docker API types only.
package dockerclone

import (
	"net/netip"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// MountPointToMount turns an inspected mount point back into a mount spec.
func MountPointToMount(mp container.MountPoint) mount.Mount {
	src := mp.Source
	if mp.Type == mount.TypeVolume && mp.Name != "" {
		src = mp.Name
	}
	m := mount.Mount{
		Type:     mp.Type,
		Source:   src,
		Target:   mp.Destination,
		ReadOnly: !mp.RW,
	}
	if mp.Type == mount.TypeBind && mp.Propagation != "" {
		m.BindOptions = &mount.BindOptions{Propagation: mp.Propagation}
	}
	return m
}

// ClonedContainerConfig holds the cloned Docker create options and any additional
// networks that must be attached after container creation.
type ClonedContainerConfig struct {
	Create        client.ContainerCreateOptions
	ExtraNetworks map[string]*network.EndpointSettings
	Compose       bool
}

// CloneContainerConfig copies the target container's runtime configuration onto newImage,
// preserving compose labels, env, mounts (including anonymous volumes), ports, restart
// policy, capabilities, security options, healthcheck, and all attached networks/aliases.
func CloneContainerConfig(insp container.InspectResponse, newImage string) ClonedContainerConfig {
	var out ClonedContainerConfig
	var cfg container.Config
	if insp.Config != nil {
		c := *insp.Config
		c.Image = newImage
		if c.Env != nil {
			c.Env = append([]string(nil), c.Env...)
		}
		if c.Cmd != nil {
			c.Cmd = append([]string(nil), c.Cmd...)
		}
		if c.Entrypoint != nil {
			c.Entrypoint = append([]string(nil), c.Entrypoint...)
		}
		if c.Shell != nil {
			c.Shell = append([]string(nil), c.Shell...)
		}
		if c.ExposedPorts != nil {
			ep := make(network.PortSet, len(c.ExposedPorts))
			for k, v := range c.ExposedPorts {
				ep[k] = v
			}
			c.ExposedPorts = ep
		}
		if c.Volumes != nil {
			vols := make(map[string]struct{}, len(c.Volumes))
			for k, v := range c.Volumes {
				vols[k] = v
			}
			c.Volumes = vols
		}
		if c.Labels != nil {
			lbl := make(map[string]string, len(c.Labels))
			for k, v := range c.Labels {
				lbl[k] = v
			}
			c.Labels = lbl
			out.Compose = lbl["com.docker.compose.project"] != ""
		}
		if c.Healthcheck != nil {
			hc := *c.Healthcheck
			if hc.Test != nil {
				hc.Test = append([]string(nil), hc.Test...)
			}
			c.Healthcheck = &hc
		}
		if c.StopTimeout != nil {
			st := *c.StopTimeout
			c.StopTimeout = &st
		}
		// Docker sets Hostname to container.ID[:12] by default when none was configured; clear
		// that synthetic hostname so the new container gets its own ID as hostname instead of
		// impersonating the old container ID. Keep any explicit custom Hostname.
		shortID := insp.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		if shortID != "" && c.Hostname == shortID {
			c.Hostname = ""
		}
		cfg = c
	} else {
		cfg.Image = newImage
	}

	var hc container.HostConfig
	if insp.HostConfig != nil {
		h := *insp.HostConfig
		h.Binds = append([]string(nil), h.Binds...)
		h.Mounts = append([]mount.Mount(nil), h.Mounts...)
		h.CapAdd = append([]string(nil), h.CapAdd...)
		h.CapDrop = append([]string(nil), h.CapDrop...)
		h.SecurityOpt = append([]string(nil), h.SecurityOpt...)
		h.GroupAdd = append([]string(nil), h.GroupAdd...)
		h.DNS = append([]netip.Addr(nil), h.DNS...)
		h.DNSOptions = append([]string(nil), h.DNSOptions...)
		h.DNSSearch = append([]string(nil), h.DNSSearch...)
		h.ExtraHosts = append([]string(nil), h.ExtraHosts...)
		h.Links = append([]string(nil), h.Links...)
		h.MaskedPaths = append([]string(nil), h.MaskedPaths...)
		h.ReadonlyPaths = append([]string(nil), h.ReadonlyPaths...)
		h.VolumesFrom = append([]string(nil), h.VolumesFrom...)
		if h.PortBindings != nil {
			pb := make(network.PortMap, len(h.PortBindings))
			for k, v := range h.PortBindings {
				pb[k] = append([]network.PortBinding(nil), v...)
			}
			h.PortBindings = pb
		}
		if h.Tmpfs != nil {
			tm := make(map[string]string, len(h.Tmpfs))
			for k, v := range h.Tmpfs {
				tm[k] = v
			}
			h.Tmpfs = tm
		}
		if h.Sysctls != nil {
			sc := make(map[string]string, len(h.Sysctls))
			for k, v := range h.Sysctls {
				sc[k] = v
			}
			h.Sysctls = sc
		}
		if h.StorageOpt != nil {
			so := make(map[string]string, len(h.StorageOpt))
			for k, v := range h.StorageOpt {
				so[k] = v
			}
			h.StorageOpt = so
		}
		if h.Annotations != nil {
			an := make(map[string]string, len(h.Annotations))
			for k, v := range h.Annotations {
				an[k] = v
			}
			h.Annotations = an
		}
		if h.LogConfig.Config != nil {
			lc := make(map[string]string, len(h.LogConfig.Config))
			for k, v := range h.LogConfig.Config {
				lc[k] = v
			}
			h.LogConfig.Config = lc
		}
		if h.Umask != nil {
			u := *h.Umask
			h.Umask = &u
		}
		if h.Init != nil {
			b := *h.Init
			h.Init = &b
		}
		hc = h
	}

	// Preserve anonymous volumes present on insp.Mounts that are not already named in Binds or Mounts.
	for _, m := range insp.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" {
			continue
		}
		if mountTargetPresent(hc.Binds, hc.Mounts, m.Destination) {
			continue
		}
		spec := m.Name + ":" + m.Destination
		if !m.RW {
			spec += ":ro"
		}
		hc.Binds = append(hc.Binds, spec)
	}

	// Build primary NetworkingConfig + extra networks. Skip for host/none/container:* network modes.
	var netCfg *network.NetworkingConfig
	if !hc.NetworkMode.IsHost() && !hc.NetworkMode.IsNone() && !hc.NetworkMode.IsContainer() &&
		insp.NetworkSettings != nil && len(insp.NetworkSettings.Networks) > 0 {
		names := make([]string, 0, len(insp.NetworkSettings.Networks))
		for n := range insp.NetworkSettings.Networks {
			names = append(names, n)
		}
		sort.Strings(names)
		primary := string(hc.NetworkMode)
		if _, ok := insp.NetworkSettings.Networks[primary]; !ok || primary == "" || primary == "default" {
			if _, ok := insp.NetworkSettings.Networks["bridge"]; ok && (primary == "" || primary == "default") {
				primary = "bridge"
			} else {
				primary = names[0]
			}
		}
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				primary: cloneEndpointSettings(insp.NetworkSettings.Networks[primary], insp.ID),
			},
		}
		for _, n := range names {
			if n == primary {
				continue
			}
			if out.ExtraNetworks == nil {
				out.ExtraNetworks = make(map[string]*network.EndpointSettings)
			}
			out.ExtraNetworks[n] = cloneEndpointSettings(insp.NetworkSettings.Networks[n], insp.ID)
		}
	}

	out.Create = client.ContainerCreateOptions{
		Name:             strings.TrimPrefix(insp.Name, "/"),
		Config:           &cfg,
		HostConfig:       &hc,
		NetworkingConfig: netCfg,
	}
	return out
}

func mountTargetPresent(binds []string, mounts []mount.Mount, target string) bool {
	for _, m := range mounts {
		if m.Target == target {
			return true
		}
	}
	for _, b := range binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 && parts[1] == target {
			return true
		}
	}
	return false
}

func cloneEndpointSettings(ep *network.EndpointSettings, oldContainerID string) *network.EndpointSettings {
	if ep == nil {
		return &network.EndpointSettings{}
	}
	c := *ep
	// Strip runtime-assigned operational fields so the engine assigns fresh IDs/IPs/MACs cleanly.
	c.NetworkID = ""
	c.EndpointID = ""
	c.Gateway = netip.Addr{}
	c.IPAddress = netip.Addr{}
	c.IPPrefixLen = 0
	c.IPv6Gateway = netip.Addr{}
	c.GlobalIPv6Address = netip.Addr{}
	c.GlobalIPv6PrefixLen = 0
	c.DNSNames = nil
	c.MacAddress = nil

	if ep.IPAMConfig != nil {
		ipam := *ep.IPAMConfig
		if ipam.LinkLocalIPs != nil {
			ipam.LinkLocalIPs = append([]netip.Addr(nil), ipam.LinkLocalIPs...)
		}
		c.IPAMConfig = &ipam
	}
	if ep.Links != nil {
		c.Links = append([]string(nil), ep.Links...)
	}
	if ep.DriverOpts != nil {
		d := make(map[string]string, len(ep.DriverOpts))
		for k, v := range ep.DriverOpts {
			d[k] = v
		}
		c.DriverOpts = d
	}
	shortID := oldContainerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	if ep.Aliases != nil {
		var aliases []string
		for _, a := range ep.Aliases {
			if a == oldContainerID || (shortID != "" && a == shortID) {
				continue
			}
			aliases = append(aliases, a)
		}
		c.Aliases = aliases
	}
	return &c
}

