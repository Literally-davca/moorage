package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// A multi-platform image (":latest" of most images, Moorage's own included) has an index
// digest, which DistributionInspect returns and a pull by tag records in RepoDigests, and
// one digest per platform manifest. An image pulled by its platform digest (Moorage's
// self-update pulls image@sha256:<platform digest> from the signed manifest) records only
// that digest, so comparing it with the index digest reported an update that was already
// installed. Docker's DistributionInspect lists the platforms of an index but not their
// digests, so when the index digest doesn't match, Moorage asks the registry for the index
// once and accepts a match on any of its platform manifests.

// PlatformDigester is an optional ImageBackend method: the platform manifest digests of
// ref's image index. It returns nil (and no error) for a single-platform image.
type PlatformDigester interface {
	RemotePlatformDigests(ctx context.Context, ref string, creds RegistryCreds) ([]string, error)
}

func (m mobyImageBackend) RemotePlatformDigests(ctx context.Context, ref string, creds RegistryCreds) ([]string, error) {
	return RemotePlatformDigestsFromRegistry(ctx, ref, creds, nil)
}

// maxIndexBytes bounds the image index Moorage reads (real ones are a few KB).
const maxIndexBytes = 1 << 20

// RemotePlatformDigestsFromRegistry GETs ref's manifest and, when it is an image index or
// manifest list, returns the digests of its manifests (attestation manifests included:
// they can never match a pulled image, so they need no filtering).
func RemotePlatformDigestsFromRegistry(ctx context.Context, ref string, creds RegistryCreds, clientOverride *http.Client) ([]string, error) {
	cl := registryHTTPClient
	if clientOverride != nil {
		cl = clientOverride
	}
	regHost, repo, tag := ParseImageRef(ref)
	path := fmt.Sprintf("/v2/%s/manifests/%s", repo, tag)
	get := func(base, bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Moorage")
		req.Header.Set("Accept", manifestAccept)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		} else if creds.Username != "" || creds.Password != "" {
			req.SetBasicAuth(creds.Username, creds.Password)
		}
		return cl.Do(req)
	}
	base := "https://" + regHost
	resp, err := get(base, "")
	if err != nil && isLocalRegistryHost(regHost) {
		base = "http://" + regHost
		resp, err = get(base, "")
	}
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		tok, terr := fetchBearerToken(ctx, cl, challenge, creds)
		if terr != nil {
			return nil, terr
		}
		if resp, err = get(base, tok); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry answered %d for the manifest", resp.StatusCode)
	}
	return parseIndexDigests(io.LimitReader(resp.Body, maxIndexBytes))
}

func parseIndexDigests(r io.Reader) ([]string, error) {
	var idx struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return nil, fmt.Errorf("read image index: %w", err)
	}
	var out []string
	for _, m := range idx.Manifests {
		if digestRe.MatchString(m.Digest) {
			out = append(out, m.Digest)
		}
	}
	return out, nil
}

// platformCache keeps the platform digests per index digest: an index never changes its
// manifests, so an entry stays valid as long as the index digest is the same.
type platformCache struct {
	mu sync.Mutex
	m  map[string]platformEntry // key: ref + "@" + index digest
}

type platformEntry struct {
	at      time.Time
	digests []string
	failed  bool
}

// platformFailTTL is how long a failed index lookup is not retried.
const platformFailTTL = 10 * time.Minute

// platformDigests returns the platform digests of ref's index (cached). Any problem
// returns nil: the caller then keeps the plain index comparison (the safe fallback for
// registries without an index, private ones with odd auth, or rate limits).
func (e *Engine) platformDigests(ctx context.Context, b ImageBackend, ref, indexDigest string) []string {
	pd, ok := b.(PlatformDigester)
	if !ok || indexDigest == "" {
		return nil
	}
	key := ref + "@" + indexDigest
	e.platforms.mu.Lock()
	if c, ok := e.platforms.m[key]; ok && (!c.failed || time.Since(c.at) < platformFailTTL) {
		e.platforms.mu.Unlock()
		return c.digests
	}
	e.platforms.mu.Unlock()

	creds, err := readRegistryAuth(ref)
	if err != nil {
		return nil
	}
	if !e.acquireImageSem(ctx) {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, imageReqTimeout)
	digests, err := pd.RemotePlatformDigests(rctx, ref, creds)
	cancel()
	e.releaseImageSem()

	ent := platformEntry{at: time.Now(), digests: digests, failed: err != nil}
	e.platforms.mu.Lock()
	if e.platforms.m == nil {
		e.platforms.m = map[string]platformEntry{}
	}
	if len(e.platforms.m) > 1000 {
		e.platforms.m = map[string]platformEntry{} // bounded; refilled on demand
	}
	e.platforms.m[key] = ent
	e.platforms.mu.Unlock()
	if err != nil {
		return nil
	}
	return digests
}

// matchesRemote tells whether a local image (its RepoDigests) is the remote one: the
// index digest itself, or, when the index lookup worked, one of its platform manifests.
func matchesRemote(repoDigests []string, indexDigest string, platforms []string) bool {
	if hasMatchingDigest(repoDigests, indexDigest) {
		return true
	}
	for _, p := range platforms {
		if hasMatchingDigest(repoDigests, p) {
			return true
		}
	}
	return false
}
