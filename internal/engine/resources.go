package engine

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// HostResources is the disk and memory use of a host (M19M).
type HostResources struct {
	Disks []DiskUse `json:"disks"`
	// MemTotal and MemAvailable in bytes; 0 when unknown.
	MemTotal     int64   `json:"memTotal,omitempty"`
	MemAvailable int64   `json:"memAvailable,omitempty"`
	MemUsedPct   float64 `json:"memUsedPct,omitempty"`
}

// DiskUse is one file system.
type DiskUse struct {
	Label   string  `json:"label"`
	Path    string  `json:"path"`
	Total   int64   `json:"total"`
	Free    int64   `json:"free"`
	UsedPct float64 `json:"usedPct"`
}

// Resources reads the disks Moorage uses (the container's root holds Docker's data in
// a container; the stacks and data folders are bind mounts of the host) and
// /proc/meminfo. File systems seen twice are reported once.
func (e *Engine) Resources() *HostResources {
	r := &HostResources{Disks: []DiskUse{}}
	seen := map[[2]int64]bool{}
	for _, d := range []struct{ label, path string }{
		{"System / Docker data", "/"},
		{"Stacks folder", e.cfg.StacksDir},
		{"Moorage data", e.cfg.DataDir},
	} {
		if d.path == "" {
			continue
		}
		total, free := diskTotalFree(d.path)
		if total <= 0 {
			continue
		}
		key := [2]int64{total, free / (64 << 20)} // same disk ≈ same size and free space
		if seen[key] {
			continue
		}
		seen[key] = true
		r.Disks = append(r.Disks, DiskUse{Label: d.label, Path: d.path, Total: total, Free: free, UsedPct: float64(total-free) / float64(total) * 100})
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			switch k {
			case "MemTotal":
				r.MemTotal = n * 1024
			case "MemAvailable":
				r.MemAvailable = n * 1024
			}
		}
		f.Close()
		if r.MemTotal > 0 {
			r.MemUsedPct = float64(r.MemTotal-r.MemAvailable) / float64(r.MemTotal) * 100
		}
	}
	if len(r.Disks) == 0 && r.MemTotal == 0 {
		return nil
	}
	return r
}

// DiskFreePct is the free share of the system disk, -1 when unknown (Problems).
func (r *HostResources) DiskFreePct() float64 {
	if r == nil || len(r.Disks) == 0 {
		return -1
	}
	return 100 - r.Disks[0].UsedPct
}
