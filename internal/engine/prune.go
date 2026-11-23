package engine

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// PrunePreview summarizes unused Docker objects and estimated reclaimable bytes.
type PrunePreview struct {
	ImagesCount     int   `json:"imagesCount"`
	ImagesBytes     int64 `json:"imagesBytes"`
	VolumesCount    int   `json:"volumesCount"`
	VolumesBytes    int64 `json:"volumesBytes"`
	ContainersCount int   `json:"containersCount"`
	ContainersBytes int64 `json:"containersBytes"`
	BuildCacheBytes int64 `json:"buildCacheBytes"`
	TotalBytes      int64 `json:"totalBytes"`
}

// PruneRequest selects which categories of unused Docker objects to remove.
type PruneRequest struct {
	Images     bool `json:"images"`
	Volumes    bool `json:"volumes"`
	Containers bool `json:"containers"`
	BuildCache bool `json:"buildCache"`
}

// PruneResult reports how much space was freed and what was removed.
type PruneResult struct {
	ReclaimedBytes uint64   `json:"reclaimedBytes"`
	Deleted        []string `json:"deleted,omitempty"`
}

// PrunePreview inspects Docker disk usage and estimates reclaimable space for unused items.
func (e *Engine) PrunePreview(ctx context.Context) (PrunePreview, error) {
	var out PrunePreview
	du, err := e.api.DiskUsage(ctx, client.DiskUsageOptions{
		Images:     true,
		Containers: true,
		Volumes:    true,
		BuildCache: true,
	})
	if err != nil {
		return out, err
	}
	for _, img := range du.Images.Items {
		if img.Containers == 0 {
			out.ImagesCount++
			out.ImagesBytes += img.Size
		}
	}
	for _, c := range du.Containers.Items {
		if c.State != "running" {
			out.ContainersCount++
			if c.SizeRw > 0 {
				out.ContainersBytes += c.SizeRw
			}
		}
	}
	for _, v := range du.Volumes.Items {
		if v.UsageData != nil && v.UsageData.RefCount == 0 {
			out.VolumesCount++
			if v.UsageData.Size > 0 {
				out.VolumesBytes += v.UsageData.Size
			}
		}
	}
	for _, bc := range du.BuildCache.Items {
		if !bc.InUse && bc.Size > 0 {
			out.BuildCacheBytes += bc.Size
		}
	}
	out.TotalBytes = out.ImagesBytes + out.ContainersBytes + out.VolumesBytes + out.BuildCacheBytes
	return out, nil
}

// PruneDisk removes selected unused Docker objects and returns reclaimed bytes.
func (e *Engine) PruneDisk(ctx context.Context, req PruneRequest) (PruneResult, error) {
	var out PruneResult
	if req.Containers {
		res, err := e.api.ContainerPrune(ctx, client.ContainerPruneOptions{})
		if err != nil {
			return out, fmt.Errorf("prune containers: %w", err)
		}
		out.ReclaimedBytes += res.Report.SpaceReclaimed
		for _, id := range res.Report.ContainersDeleted {
			out.Deleted = append(out.Deleted, "container:"+id[:min(12, len(id))])
		}
	}
	if req.Images {
		res, err := e.api.ImagePrune(ctx, client.ImagePruneOptions{
			Filters: make(client.Filters).Add("dangling", "false"),
		})
		if err != nil {
			return out, fmt.Errorf("prune images: %w", err)
		}
		out.ReclaimedBytes += res.Report.SpaceReclaimed
		for _, d := range res.Report.ImagesDeleted {
			if d.Deleted != "" {
				out.Deleted = append(out.Deleted, "image:"+d.Deleted)
			} else if d.Untagged != "" {
				out.Deleted = append(out.Deleted, "untagged:"+d.Untagged)
			}
		}
	}
	if req.Volumes {
		res, err := e.api.VolumePrune(ctx, client.VolumePruneOptions{})
		if err != nil {
			return out, fmt.Errorf("prune volumes: %w", err)
		}
		out.ReclaimedBytes += res.Report.SpaceReclaimed
		for _, id := range res.Report.VolumesDeleted {
			out.Deleted = append(out.Deleted, "volume:"+id)
		}
	}
	if req.BuildCache {
		res, err := e.api.BuildCachePrune(ctx, client.BuildCachePruneOptions{})
		if err != nil {
			return out, fmt.Errorf("prune build cache: %w", err)
		}
		out.ReclaimedBytes += res.Report.SpaceReclaimed
		for _, id := range res.Report.CachesDeleted {
			out.Deleted = append(out.Deleted, "cache:"+id)
		}
	}
	return out, nil
}
