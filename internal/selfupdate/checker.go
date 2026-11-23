package selfupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"moorage/internal/host"
	"moorage/internal/netguard"
)

const (
	checkInterval = 24 * time.Hour
	httpTimeout   = 15 * time.Second
	dlTimeout     = 5 * time.Minute
	notesMaxRunes = 600
)

// ReleaseAsset describes one downloadable binary or metadata file on a release.
type ReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ReleaseInfo holds the verified metadata of the newest eligible Moorage release.
type ReleaseInfo struct {
	Version     string            `json:"version"`
	Tag         string            `json:"tag"`
	PreRelease  bool              `json:"prerelease"`
	PublishedAt time.Time         `json:"publishedAt,omitzero"`
	HTMLURL     string            `json:"htmlUrl,omitempty"`
	Notes       string            `json:"notes,omitempty"`
	NotesFull   bool              `json:"notesTruncated,omitempty"`
	Image       string            `json:"image,omitempty"`
	IndexDigest string            `json:"indexDigest,omitempty"`
	Digests     map[string]string `json:"digests,omitempty"`
	MinHub      string            `json:"minHub,omitempty"`
	MinAgent    string            `json:"minAgent,omitempty"`
	Assets      map[string]string `json:"-"` // filename -> download URL
	Sums        map[string]string `json:"-"` // filename -> sha256 hex
	RawSums     []byte            `json:"-"`
	RawSig      []byte            `json:"-"`
	RawImages   []byte            `json:"-"`
}

// CheckState captures the cached release check result, last attempt timestamp, and error.
type CheckState struct {
	Release     *ReleaseInfo
	LastChecked time.Time
	Error       string
}

// Checker fetches, verifies, and caches Moorage release metadata from MOORAGE_UPDATE_URL.
type Checker struct {
	UpdateURL string
	Channel   string // stable | pre
	Version   string // used only in User-Agent
	PubKey    ed25519.PublicKey
	Client    *http.Client // overridable in tests
	Now       func() time.Time

	mu    sync.RWMutex
	state CheckState
}

// NewChecker creates a release checker using netguard unless Client is replaced in tests.
func NewChecker(updateURL, channel, version string, pub ed25519.PublicKey) *Checker {
	tr := &http.Transport{
		DialContext:         netguard.DialContext(10*time.Second, false),
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Checker{
		UpdateURL: strings.TrimRight(updateURL, "/"),
		Channel:   channel,
		Version:   version,
		PubKey:    pub,
		Client:    &http.Client{Transport: tr, Timeout: dlTimeout},
		Now:       time.Now,
	}
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Cached returns the last known release check state without hitting the network.
func (c *Checker) Cached() CheckState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// RunPeriodic refreshes the release cache on startup (best-effort) and every 24 h.
func (c *Checker) RunPeriodic(ctx context.Context) {
	_, _ = c.Refresh(ctx)
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = c.Refresh(ctx)
		}
	}
}

// Refresh checks UpdateURL for the newest signed release matching Channel.
// On network/HTTP/404/rate-limit/verification failure, any previously cached Release is preserved.
func (c *Checker) Refresh(ctx context.Context) (CheckState, error) {
	now := c.now()
	rel, err := c.fetchLatest(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.LastChecked = now
	if err != nil {
		c.state.Error = err.Error()
		return c.state, err
	}
	c.state.Release = rel
	c.state.Error = ""
	return c.state, nil
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (c *Checker) fetchLatest(ctx context.Context) (*ReleaseInfo, error) {
	if c.UpdateURL == "" {
		return nil, fmt.Errorf("MOORAGE_UPDATE_URL is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	body, err := c.getBytes(reqCtx, c.UpdateURL, "application/vnd.github+json", MaxMetaSize*2)
	if err != nil {
		return nil, err
	}
	rels, err := decodeReleasesJSON(body)
	if err != nil {
		return nil, fmt.Errorf("parse releases feed: %w", err)
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("no releases published yet")
	}

	var chosen *ghRelease
	var chosenVer SemVer
	for i := range rels {
		r := &rels[i]
		if r.Draft {
			continue
		}
		sv, err := ParseVersion(r.TagName)
		if err != nil {
			continue
		}
		isPre := r.Prerelease || len(sv.Pre) > 0
		if c.Channel != "pre" && isPre {
			continue
		}
		if chosen == nil || compareSemVer(sv, chosenVer) > 0 {
			chosen = r
			chosenVer = sv
		}
	}
	if chosen == nil {
		if c.Channel != "pre" {
			return nil, fmt.Errorf("no stable releases found")
		}
		return nil, fmt.Errorf("no releases published yet")
	}

	assets := make(map[string]string, len(chosen.Assets))
	for _, a := range chosen.Assets {
		if a.Name != "" && a.BrowserDownloadURL != "" {
			assets[a.Name] = a.BrowserDownloadURL
		}
	}
	sumsURL := assets["SHA256SUMS"]
	sigURL := assets["SHA256SUMS.sig"]
	imagesURL := assets["images.json"]
	if sumsURL == "" || sigURL == "" || imagesURL == "" {
		return nil, fmt.Errorf("release %s is missing signed manifest assets (SHA256SUMS / .sig / images.json)", chosen.TagName)
	}

	sumsBytes, err := c.getBytes(reqCtx, sumsURL, "", MaxMetaSize)
	if err != nil {
		return nil, fmt.Errorf("fetch SHA256SUMS: %w", err)
	}
	sigBytes, err := c.getBytes(reqCtx, sigURL, "", 4096)
	if err != nil {
		return nil, fmt.Errorf("fetch SHA256SUMS.sig: %w", err)
	}
	imagesBytes, err := c.getBytes(reqCtx, imagesURL, "", MaxMetaSize)
	if err != nil {
		return nil, fmt.Errorf("fetch images.json: %w", err)
	}

	pub := c.PubKey
	if len(pub) == 0 {
		pub, err = DefaultPublicKey()
		if err != nil {
			return nil, err
		}
	}
	manifest := host.SelfUpdateManifest{
		Version:    chosenVer.Raw,
		Sums:       sumsBytes,
		Sig:        sigBytes,
		ImagesJSON: imagesBytes,
	}
	sums, img, err := VerifyManifest(manifest, pub)
	if err != nil {
		return nil, err
	}

	excerpt, truncated := trimExcerpt(chosen.Body, notesMaxRunes)
	return &ReleaseInfo{
		Version:     chosenVer.Raw,
		Tag:         chosen.TagName,
		PreRelease:  chosen.Prerelease || len(chosenVer.Pre) > 0,
		PublishedAt: chosen.PublishedAt,
		HTMLURL:     chosen.HTMLURL,
		Notes:       excerpt,
		NotesFull:   truncated,
		Image:       img.Image,
		IndexDigest: img.IndexDigest,
		Digests:     img.Digests,
		MinHub:      img.MinHub,
		MinAgent:    img.MinAgent,
		Assets:      assets,
		Sums:        sums,
		RawSums:     sumsBytes,
		RawSig:      sigBytes,
		RawImages:   imagesBytes,
	}, nil
}

func decodeReleasesJSON(body []byte) ([]ghRelease, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var list []ghRelease
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var single ghRelease
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, err
		}
		if single.TagName == "" {
			return nil, nil
		}
		return []ghRelease{single}, nil
	}
	return nil, fmt.Errorf("unexpected JSON payload")
}

func (c *Checker) getBytes(ctx context.Context, rawURL, accept string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	// Privacy/no-telemetry guarantee: only User-Agent with Moorage/<version> is sent.
	uaVer := c.Version
	if uaVer == "" {
		uaVer = "dev"
	}
	req.Header.Set("User-Agent", "Moorage/"+uaVer)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
}

func checkHTTPStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("no releases found (HTTP 404)")
	case http.StatusForbidden, http.StatusTooManyRequests:
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return fmt.Errorf("release check rate limited (resets at %s)", time.Unix(ts, 0).Local().Format("15:04"))
			}
		}
		return fmt.Errorf("release check rate limited (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("release server returned HTTP %d", resp.StatusCode)
	}
}

// DownloadBinaryToTemp downloads assetName from rel into tempDir, streaming to disk and
// verifying its SHA-256 against rel.Sums[assetName] without buffering the binary in RAM.
func (c *Checker) DownloadBinaryToTemp(ctx context.Context, rel *ReleaseInfo, assetName, tempDir string) (string, error) {
	if rel == nil {
		return "", fmt.Errorf("%w: no release loaded", host.ErrSelfUpdateInvalid)
	}
	assetURL := rel.Assets[assetName]
	wantHex := rel.Sums[assetName]
	if assetURL == "" || wantHex == "" {
		return "", fmt.Errorf("%w: release %s has no signed binary for %s", host.ErrSelfUpdateInvalid, rel.Version, assetName)
	}
	dlCtx, cancel := context.WithTimeout(ctx, dlTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, assetURL, nil)
	if err != nil {
		return "", err
	}
	uaVer := c.Version
	if uaVer == "" {
		uaVer = "dev"
	}
	req.Header.Set("User-Agent", "Moorage/"+uaVer)

	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkHTTPStatus(resp); err != nil {
		return "", err
	}
	return VerifyReaderToTemp(tempDir, resp.Body, wantHex, MaxBinarySize)
}

// CleanupTemp removes a temporary downloaded file cleanly.
func CleanupTemp(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func trimExcerpt(body string, maxRunes int) (string, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if s == "" {
		return "", false
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s, false
	}
	return strings.TrimSpace(string(r[:maxRunes])) + "…", true
}
