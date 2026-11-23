package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// SelfUpdateMaxBinarySize caps binary update uploads streamed over the tunnel (64 MiB).
const SelfUpdateMaxBinarySize = 64 << 20

// SelfUpdateStatus describes how a Moorage node (hub or agent) is running and whether it
// can update itself.
type SelfUpdateStatus struct {
	Mode           string    `json:"mode"` // container | binary | unknown
	Reason         string    `json:"reason,omitempty"`
	Supported      bool      `json:"supported"`
	SelfUpdate     bool      `json:"selfUpdate"`
	Arch           string    `json:"arch,omitempty"`
	Platform       string    `json:"platform,omitempty"` // linux/amd64, linux/arm64, linux/arm/v7
	Version        string    `json:"version,omitempty"`
	PrevVersion    string    `json:"prevVersion,omitempty"`
	PrevSavedAt    time.Time `json:"prevSavedAt,omitzero"`
	ComposeProject string    `json:"composeProject,omitempty"`
	ContainerName  string    `json:"containerName,omitempty"`
	Image          string    `json:"image,omitempty"`
	LastRollback   string    `json:"lastRollback,omitempty"`
}

// SelfUpdateManifest carries the signed release verification bundle sent from the hub to
// the installing node. The installing node verifies Sig against its own embedded Ed25519
// public key before touching disk or containers.
type SelfUpdateManifest struct {
	Version    string `json:"version"`
	Sums       []byte `json:"sums"`
	Sig        []byte `json:"sig"`
	ImagesJSON []byte `json:"imagesJson"`
	Image      string `json:"image,omitempty"`  // container repo (e.g. ghcr.io/literally-davca/moorage)
	Digest     string `json:"digest,omitempty"` // per-arch manifest digest (sha256:...)
}

// AgentSelfUpdater handles self-update and rollback requests locally on a node (agent or hub).
type AgentSelfUpdater interface {
	Status(ctx context.Context) (SelfUpdateStatus, error)
	ApplyBinary(ctx context.Context, m SelfUpdateManifest, binary io.Reader) error
	ApplyContainer(ctx context.Context, m SelfUpdateManifest) error
	Rollback(ctx context.Context) error
	ConfirmHealthy(version string)
}

// SelfUpdateStatus asks an agent for its self-update readiness, mode, and kept previous version.
// Older agents return 404 (host.IsUnsupported).
func (rm *Remote) SelfUpdateStatus(ctx context.Context) (SelfUpdateStatus, error) {
	var st SelfUpdateStatus
	err := rm.call(ctx, http.MethodGet, "/system/update/status", nil, &st)
	return st, err
}

// ApplyBinaryUpdate streams the signed manifest and Moorage binary over the tunnel to an agent.
func (rm *Remote) ApplyBinaryUpdate(ctx context.Context, m SelfUpdateManifest, bin io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	writeErr := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			_ = mw.Close()
			if err != nil {
				_ = pw.CloseWithError(err)
			} else {
				_ = pw.Close()
			}
			writeErr <- err
		}()
		mf, werr := mw.CreateFormField("manifest")
		if werr != nil {
			err = werr
			return
		}
		if werr := json.NewEncoder(mf).Encode(m); werr != nil {
			err = werr
			return
		}
		bf, werr := mw.CreateFormFile("binary", "moorage")
		if werr != nil {
			err = werr
			return
		}
		if _, werr := io.Copy(bf, bin); werr != nil {
			err = werr
			return
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentBase+"/system/update/apply-binary", pr)
	if err != nil {
		_ = pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := rm.c.Do(req)
	_ = pr.Close()
	<-writeErr
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrOffline, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Code, Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if resp.StatusCode == http.StatusNotFound && e.Code == "" {
			return errRouteNotFound
		}
		return FromCode(e.Code, e.Error)
	}
	return nil
}

// ApplyContainerUpdate sends the signed manifest and image+digest to a containerized agent.
func (rm *Remote) ApplyContainerUpdate(ctx context.Context, m SelfUpdateManifest) error {
	return rm.callTimeout(ctx, 5*time.Minute, http.MethodPost, "/system/update/apply-container", m, nil)
}

// RollbackSelfUpdate asks an agent to restore its kept previous binary or container.
func (rm *Remote) RollbackSelfUpdate(ctx context.Context) error {
	return rm.callTimeout(ctx, 2*time.Minute, http.MethodPost, "/system/update/rollback", nil, nil)
}

func handleAgentApplyBinary(w http.ResponseWriter, r *http.Request, su AgentSelfUpdater) {
	mr, err := r.MultipartReader()
	if err != nil {
		agentErr(w, fmt.Errorf("%w: invalid multipart upload: %v", ErrSelfUpdateInvalid, err))
		return
	}
	var manifest *SelfUpdateManifest
	applied := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			agentErr(w, fmt.Errorf("%w: read upload: %v", ErrSelfUpdateInvalid, err))
			return
		}
		switch part.FormName() {
		case "manifest":
			var m SelfUpdateManifest
			decErr := json.NewDecoder(io.LimitReader(part, 512<<10)).Decode(&m)
			_ = part.Close()
			if decErr != nil {
				agentErr(w, fmt.Errorf("%w: bad manifest: %v", ErrSelfUpdateInvalid, decErr))
				return
			}
			manifest = &m
		case "binary":
			if manifest == nil {
				_ = part.Close()
				agentErr(w, fmt.Errorf("%w: manifest must precede binary", ErrSelfUpdateInvalid))
				return
			}
			lr := io.LimitReader(part, SelfUpdateMaxBinarySize+1)
			applyErr := su.ApplyBinary(r.Context(), *manifest, lr)
			_ = part.Close()
			if applyErr != nil {
				agentErr(w, applyErr)
				return
			}
			applied = true
		default:
			_ = part.Close()
		}
	}
	if !applied {
		agentErr(w, fmt.Errorf("%w: missing binary part", ErrSelfUpdateInvalid))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
