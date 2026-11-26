package engine

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"moorage/internal/netguard"
)

// Image check statuses.
const (
	ImageUpToDate         = "up-to-date"
	ImageUpdateAvailable  = "update-available"
	ImagePulledNotApplied = "pulled-not-applied"
	ImagePinned           = "pinned"
	ImageBuilt            = "built"
	ImageLocalOnly        = "local-only"
	ImageUnreachable      = "unreachable"
	ImageUnsupported      = "unsupported"
)

const (
	imageReqTimeout  = 20 * time.Second
	imageMaxParallel = 4
	imageCacheTTL    = 25 * time.Second
	maxRemoteTags    = 10_000
)

// ErrManualEdit is returned when a compose image line can't be safely rewritten automatically.
var ErrManualEdit = errors.New("Edit compose.yaml manually")

// ServiceImage is the image check result for one service of a stack.
type ServiceImage struct {
	Service      string `json:"service"`
	Ref          string `json:"ref"`
	LocalID      string `json:"localId,omitempty"`
	LocalDigest  string `json:"localDigest,omitempty"`
	RunningID    string `json:"runningId,omitempty"`
	RemoteDigest string `json:"remoteDigest,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	NewerTag     string `json:"newerTag,omitempty"`
	NewerMajor   string `json:"newerMajor,omitempty"`
}

// RunningImage describes the local and running image of a service before/after an update.
type RunningImage struct {
	Service     string   `json:"service"`
	Ref         string   `json:"ref"`
	ImageID     string   `json:"imageId"`
	RepoDigests []string `json:"repoDigests,omitempty"`
	Digest      string   `json:"digest,omitempty"`
}

// RollbackTarget specifies the previous image to restore for one service.
type RollbackTarget struct {
	Ref       string `json:"ref"`
	OldID     string `json:"oldId"`
	OldDigest string `json:"oldDigest,omitempty"`
}

// RegistryCreds holds decoded credentials from ~/.docker/config.json auths.
type RegistryCreds struct {
	Username      string
	Password      string
	ServerAddress string
	EncodedAuth   string
}

// ImageBackend abstracts Docker daemon and registry HTTP calls so unit tests can use fakes.
type ImageBackend interface {
	InspectLocal(ctx context.Context, ref string) (id string, repoDigests []string, found bool, err error)
	InspectRemote(ctx context.Context, ref string, creds RegistryCreds) (digest string, err error)
	SelectRemoteTags(ctx context.Context, ref, currentTag string, creds RegistryCreds) (sameMajor, newerMajor string, err error)
	PullImage(ctx context.Context, ref string, creds RegistryCreds) error
	TagImage(ctx context.Context, sourceID, targetRef string) error
	RunningContainers(ctx context.Context, stack string) ([]Container, error)
}

type cachedRemoteImage struct {
	at         time.Time
	digest     string
	err        string
	newerTag   string
	newerMajor string
}

type mobyImageBackend struct {
	e *Engine
}

func (e *Engine) imageBackend() ImageBackend {
	if e.ImageBackend != nil {
		return e.ImageBackend
	}
	return mobyImageBackend{e: e}
}

func (m mobyImageBackend) InspectLocal(ctx context.Context, ref string) (string, []string, bool, error) {
	res, err := m.e.api.ImageInspect(ctx, ref)
	if err != nil {
		if errdefs.IsNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "no such image") || strings.Contains(strings.ToLower(err.Error()), "not found") {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	return res.ID, res.RepoDigests, true, nil
}

func (m mobyImageBackend) InspectRemote(ctx context.Context, ref string, creds RegistryCreds) (string, error) {
	res, err := m.e.api.DistributionInspect(ctx, ref, client.DistributionInspectOptions{
		EncodedRegistryAuth: creds.EncodedAuth,
	})
	if err != nil && m.e.isPodman(ctx) {
		// Podman's compat API had no /distribution/{name}/json until recently
		// (https://github.com/containers/podman/issues/17726), so ask the registry directly.
		return RemoteDigestFromRegistry(ctx, ref, creds, nil)
	}
	if err != nil {
		return "", err
	}
	d := string(res.Descriptor.Digest)
	if d == "" {
		return "", errors.New("empty digest from registry")
	}
	return d, nil
}

func (m mobyImageBackend) SelectRemoteTags(ctx context.Context, ref, currentTag string, creds RegistryCreds) (string, string, error) {
	return SelectNewerTagsFromRegistry(ctx, ref, currentTag, creds, nil)
}

func (m mobyImageBackend) PullImage(ctx context.Context, ref string, creds RegistryCreds) error {
	rc, err := m.e.api.ImagePull(ctx, ref, client.ImagePullOptions{
		RegistryAuth: creds.EncodedAuth,
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (m mobyImageBackend) TagImage(ctx context.Context, sourceID, targetRef string) error {
	_, err := m.e.api.ImageTag(ctx, client.ImageTagOptions{
		Source: sourceID,
		Target: targetRef,
	})
	return err
}

func (m mobyImageBackend) RunningContainers(ctx context.Context, stack string) ([]Container, error) {
	byProj, _, err := m.e.projectContainers(ctx, stack)
	if err != nil {
		return nil, err
	}
	return byProj[stack], nil
}

func (e *Engine) acquireImageSem(ctx context.Context) bool {
	e.imgMu.Lock()
	if e.imgSem == nil {
		e.imgSem = make(chan struct{}, imageMaxParallel)
	}
	sem := e.imgSem
	e.imgMu.Unlock()
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *Engine) releaseImageSem() {
	e.imgMu.Lock()
	sem := e.imgSem
	e.imgMu.Unlock()
	if sem != nil {
		<-sem
	}
}

// ResetImageCache clears the short-lived remote lookup deduplication cache.
func (e *Engine) ResetImageCache() {
	e.imgMu.Lock()
	e.imgCache = nil
	e.imgMu.Unlock()
}

type stackServiceSpec struct {
	Service string
	Ref     string
	Built   bool
}

func (e *Engine) loadStackServiceSpecs(ctx context.Context, stack string, containers []Container) ([]stackServiceSpec, error) {
	// Stacks outside the stacks folder (other compose projects, often with names that
	// aren't valid Moorage stack names) have no file here: their containers tell the images.
	cf := ""
	if dir, err := e.stackDir(stack); err == nil {
		cf = composeFile(dir)
	}
	if cf != "" {
		dir := filepath.Dir(cf)
		composeBytes, err := os.ReadFile(cf)
		if err != nil {
			return nil, err
		}
		envText := ""
		if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
			envText = string(b)
		}
		specs, err := parseComposeServiceSpecs(stack, string(composeBytes), envText)
		if err == nil {
			return specs, nil
		}
	}
	if len(containers) == 0 {
		return nil, ErrNotFound
	}
	seen := map[string]bool{}
	var out []stackServiceSpec
	for _, c := range containers {
		svc := c.Service
		if svc == "" {
			svc = c.Name
		}
		if seen[svc] {
			continue
		}
		seen[svc] = true
		out = append(out, stackServiceSpec{Service: svc, Ref: c.Image})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

func parseComposeServiceSpecs(name, composeText, envText string) ([]stackServiceSpec, error) {
	env := map[string]string{}
	if strings.TrimSpace(envText) != "" {
		if parsed, err := dotenv.UnmarshalBytesWithLookup([]byte(envText), nil); err == nil {
			env = parsed
		}
	}
	proj, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(composeText)}},
		Environment: env,
	}, func(o *loader.Options) {
		o.SetProjectName(name, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return nil, err
	}
	out := make([]stackServiceSpec, 0, len(proj.Services))
	for svcName, s := range proj.Services {
		out = append(out, stackServiceSpec{
			Service: svcName,
			Ref:     strings.TrimSpace(s.Image),
			Built:   s.Build != nil,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// CheckImages checks every service of a stack for new digests (Part A) and newer version tags (Part B).
func (e *Engine) CheckImages(ctx context.Context, stack string) ([]ServiceImage, error) {
	b := e.imageBackend()
	containers, _ := b.RunningContainers(ctx, stack)
	specs, err := e.loadStackServiceSpecs(ctx, stack, containers)
	if err != nil {
		return nil, err
	}
	runningByService := map[string]string{}
	for _, c := range containers {
		if c.Service != "" && c.ImageID != "" {
			if runningByService[c.Service] == "" || c.State == "running" {
				runningByService[c.Service] = c.ImageID
			}
		}
	}

	// Services are checked in parallel; lookupRemoteImage caps the registry requests at
	// imageMaxParallel. One by one, a dozen services took close to a minute.
	out := make([]ServiceImage, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = e.checkService(ctx, b, sp, runningByService[sp.Service])
		}()
	}
	wg.Wait()
	return out, nil
}

func (e *Engine) checkService(ctx context.Context, b ImageBackend, sp stackServiceSpec, runningID string) ServiceImage {
	si := ServiceImage{Service: sp.Service, Ref: sp.Ref, RunningID: runningID}
	if sp.Built || sp.Ref == "" {
		si.Status = ImageBuilt
		return si
	}
	if idx := strings.Index(sp.Ref, "@sha256:"); idx >= 0 {
		si.Status = ImagePinned
		si.LocalDigest = sp.Ref[idx+1:]
		return si
	}

	localID, repoDigests, found, lerr := b.InspectLocal(ctx, sp.Ref)
	if lerr != nil {
		si.Status = ImageUnreachable
		si.Error = classifyRegistryError(lerr)
		return si
	}
	if found {
		si.LocalID = localID
		if len(repoDigests) == 0 {
			si.Status = ImageLocalOnly
			return si
		}
		si.LocalDigest = repoDigestFor(sp.Ref, repoDigests)
	}

	// Remote lookup (deduplicated across stacks within imageCacheTTL).
	rem := e.lookupRemoteImage(ctx, b, sp.Ref)
	if rem.err != "" {
		si.Status = ImageUnreachable
		si.Error = rem.err
		return si
	}
	si.RemoteDigest = rem.digest
	si.NewerTag = rem.newerTag
	si.NewerMajor = rem.newerMajor

	if found && hasMatchingDigest(repoDigests, rem.digest) {
		si.LocalDigest = rem.digest
		if si.RunningID != "" && si.LocalID != "" && si.RunningID != si.LocalID {
			si.Status = ImagePulledNotApplied
		} else {
			si.Status = ImageUpToDate
		}
		return si
	}
	// No match with the index digest. Before reporting an update, ask (once, cached) for
	// the index's platform digests: an image pulled by its platform digest (self-update)
	// is current too. Both the local tag and the running image count: after a self-update
	// the container runs image@<platform digest> while the tag may still name the old one.
	// When the lookup fails or there is no index, platforms is nil: the plain comparison.
	var platforms []string
	if found || si.RunningID != "" {
		platforms = e.platformDigests(ctx, b, sp.Ref, rem.digest)
	}
	switch {
	case found && matchesRemote(repoDigests, rem.digest, platforms):
		si.LocalDigest = repoDigestFor(sp.Ref, repoDigests)
		if si.RunningID != "" && si.LocalID != "" && si.RunningID != si.LocalID && !e.runningMatches(ctx, b, si.RunningID, rem.digest, platforms) {
			si.Status = ImagePulledNotApplied
		} else {
			si.Status = ImageUpToDate
		}
	case si.RunningID != "" && si.RunningID != si.LocalID && e.runningMatches(ctx, b, si.RunningID, rem.digest, platforms):
		si.Status = ImageUpToDate
	default:
		si.Status = ImageUpdateAvailable
	}
	return si
}

// runningMatches tells whether the image a container runs (by ID) is the remote one.
func (e *Engine) runningMatches(ctx context.Context, b ImageBackend, runningID, indexDigest string, platforms []string) bool {
	_, digests, found, err := b.InspectLocal(ctx, runningID)
	return err == nil && found && matchesRemote(digests, indexDigest, platforms)
}

func (e *Engine) lookupRemoteImage(ctx context.Context, b ImageBackend, ref string) cachedRemoteImage {
	// Two services (or two stacks) with the same reference share one registry lookup:
	// the first caller asks, the others wait for its cached result.
	for {
		e.imgMu.Lock()
		if c, ok := e.imgCache[ref]; ok && time.Since(c.at) < imageCacheTTL {
			e.imgMu.Unlock()
			return c
		}
		wait, busy := e.imgBusy[ref]
		if !busy {
			if e.imgBusy == nil {
				e.imgBusy = map[string]chan struct{}{}
			}
			done := make(chan struct{})
			e.imgBusy[ref] = done
			e.imgMu.Unlock()
			defer func() {
				e.imgMu.Lock()
				delete(e.imgBusy, ref)
				e.imgMu.Unlock()
				close(done)
			}()
			break
		}
		e.imgMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return cachedRemoteImage{at: time.Now(), err: "timeout"}
		}
	}

	res := e.fetchRemoteImage(ctx, b, ref)
	e.storeCachedImage(ref, res)
	return res
}

func (e *Engine) fetchRemoteImage(ctx context.Context, b ImageBackend, ref string) cachedRemoteImage {
	creds, cerr := readRegistryAuth(ref)
	if cerr != nil {
		return cachedRemoteImage{at: time.Now(), err: classifyRegistryError(cerr)}
	}
	if !e.acquireImageSem(ctx) {
		return cachedRemoteImage{at: time.Now(), err: "timeout"}
	}
	defer e.releaseImageSem()

	rctx, cancel := context.WithTimeout(ctx, imageReqTimeout)
	defer cancel()
	digest, err := b.InspectRemote(rctx, ref, creds)
	if err != nil {
		return cachedRemoteImage{at: time.Now(), err: classifyRegistryError(err)}
	}

	res := cachedRemoteImage{at: time.Now(), digest: digest}
	if _, _, tag := ParseImageRef(ref); tag != "" {
		if _, ok := ParseVersionTag(tag); ok {
			tctx, tcancel := context.WithTimeout(ctx, imageReqTimeout)
			same, major, terr := b.SelectRemoteTags(tctx, ref, tag, creds)
			tcancel()
			if terr == nil {
				res.newerTag = same
				res.newerMajor = major
			}
		}
	}
	return res
}

func (e *Engine) storeCachedImage(ref string, c cachedRemoteImage) {
	e.imgMu.Lock()
	if e.imgCache == nil {
		e.imgCache = map[string]cachedRemoteImage{}
	}
	e.imgCache[ref] = c
	e.imgMu.Unlock()
}

func extractDigest(repoDigest string) string {
	if idx := strings.Index(repoDigest, "@"); idx >= 0 {
		return repoDigest[idx+1:]
	}
	return repoDigest
}

// repoDigestFor picks the repo digest of ref's own repository. An image tagged under
// several names has one entry per repository, and their order is not defined.
func repoDigestFor(ref string, repoDigests []string) string {
	if len(repoDigests) == 0 {
		return ""
	}
	reg, repo, _ := ParseImageRef(ref)
	for _, rd := range repoDigests {
		name, _, _ := strings.Cut(rd, "@")
		if r2, p2, _ := ParseImageRef(name); r2 == reg && p2 == repo {
			return extractDigest(rd)
		}
	}
	return extractDigest(repoDigests[0])
}

func hasMatchingDigest(repoDigests []string, remoteDigest string) bool {
	if remoteDigest == "" {
		return false
	}
	for _, rd := range repoDigests {
		if rd == remoteDigest || strings.HasSuffix(rd, "@"+remoteDigest) {
			return true
		}
	}
	return false
}

// StackImages returns the current local/running image IDs and digests for every service of a stack.
func (e *Engine) StackImages(ctx context.Context, stack string) (map[string]RunningImage, error) {
	b := e.imageBackend()
	containers, _ := b.RunningContainers(ctx, stack)
	specs, err := e.loadStackServiceSpecs(ctx, stack, containers)
	if err != nil {
		return nil, err
	}
	runningByService := map[string]Container{}
	for _, c := range containers {
		if c.Service != "" {
			if _, ok := runningByService[c.Service]; !ok || c.State == "running" {
				runningByService[c.Service] = c
			}
		}
	}
	out := make(map[string]RunningImage, len(specs))
	for _, sp := range specs {
		if sp.Built || sp.Ref == "" {
			continue
		}
		ri := RunningImage{Service: sp.Service, Ref: sp.Ref}
		if c, ok := runningByService[sp.Service]; ok && c.ImageID != "" {
			ri.ImageID = c.ImageID
		}
		target := sp.Ref
		if ri.ImageID != "" {
			target = ri.ImageID
		}
		id, digests, found, err := b.InspectLocal(ctx, target)
		if err == nil && found {
			if ri.ImageID == "" {
				ri.ImageID = id
			}
			ri.RepoDigests = digests
			if len(digests) > 0 {
				ri.Digest = repoDigestFor(sp.Ref, digests)
			}
		} else if id2, digests2, found2, err2 := b.InspectLocal(ctx, sp.Ref); err2 == nil && found2 {
			if ri.ImageID == "" {
				ri.ImageID = id2
			}
			ri.RepoDigests = digests2
			if len(digests2) > 0 {
				ri.Digest = repoDigestFor(sp.Ref, digests2)
			}
		}
		out[sp.Service] = ri
	}
	return out, nil
}

// RollbackImages re-tags each service's image tag back to its previous image ID (pulling by digest if pruned)
// and runs compose up without pull.
func (e *Engine) RollbackImages(ctx context.Context, stack string, targets map[string]RollbackTarget, author string) (OpInfo, error) {
	if e.IsSelf(stack) {
		return OpInfo{}, ErrSelfStack
	}
	dir, err := e.stackDir(stack)
	if err != nil {
		return OpInfo{}, err
	}
	if composeFile(dir) == "" {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return OpInfo{}, ErrNotFound
		}
		return OpInfo{}, ErrReadOnly
	}
	if !e.lock(stack) {
		return OpInfo{}, ErrBusy
	}
	op := e.ops.add(stack, "rollback")
	go func() {
		defer e.unlock(stack)
		runCtx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		err := e.execRollback(runCtx, stack, dir, targets, op)
		if err != nil {
			fmt.Fprintf(op, "✗ rollback failed: %v\n", err)
			e.log.Warn("image rollback failed", "stack", stack, "author", author, "err", err)
		} else {
			fmt.Fprintf(op, "✓ rollback finished\n")
			e.log.Info("image rollback finished", "stack", stack, "author", author)
		}
		op.finish(err)
	}()
	return op.Info(), nil
}

func (e *Engine) execRollback(ctx context.Context, stack, dir string, targets map[string]RollbackTarget, op *Op) error {
	b := e.imageBackend()
	services := make([]string, 0, len(targets))
	for svc := range targets {
		services = append(services, svc)
	}
	sort.Strings(services)

	for _, svc := range services {
		t := targets[svc]
		if t.Ref == "" || (t.OldID == "" && t.OldDigest == "") {
			continue
		}
		source := t.OldID
		found := false
		if source != "" {
			_, _, found, _ = b.InspectLocal(ctx, source)
		}
		if !found {
			if t.OldDigest == "" {
				return fmt.Errorf("old image %s for %s is no longer local and has no digest to pull", shortID(t.OldID), svc)
			}
			digestRef := RepoWithDigest(t.Ref, t.OldDigest)
			fmt.Fprintf(op, "Pulling previous image %s for %s\n", digestRef, svc)
			creds, err := readRegistryAuth(t.Ref)
			if err != nil {
				return fmt.Errorf("pull %s: %w", digestRef, err)
			}
			if err := b.PullImage(ctx, digestRef, creds); err != nil {
				return fmt.Errorf("pull %s: %w", digestRef, err)
			}
			source = digestRef
			if id, _, ok, _ := b.InspectLocal(ctx, digestRef); ok && id != "" {
				source = id
			}
		}
		fmt.Fprintf(op, "Restoring %s to %s\n", t.Ref, shortID(source))
		if err := b.TagImage(ctx, source, t.Ref); err != nil {
			return fmt.Errorf("tag %s as %s: %w", shortID(source), t.Ref, err)
		}
	}

	// If using a fake ImageBackend in tests without a real Docker daemon, skip compose up when e.cli == nil.
	if e.cli == nil {
		fmt.Fprintf(op, "Recreated %s\n", stack)
		return nil
	}
	// "up-local", not "up": a service with pull_policy: always would pull the tag again
	// and bring back the very image this rollback removes.
	return e.runCompose(ctx, op, stack, dir, composeFile(dir), "up-local")
}

func shortID(id string) string {
	s := strings.TrimPrefix(id, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// RepoWithDigest replaces the tag of ref with @digest.
func RepoWithDigest(ref, digest string) string {
	if idx := strings.Index(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	} else {
		lastSlash := strings.LastIndex(ref, "/")
		lastColon := strings.LastIndex(ref, ":")
		if lastColon > lastSlash {
			ref = ref[:lastColon]
		}
	}
	return ref + "@" + digest
}

// ---- Docker config.json credentials (auths only, no helpers) ---------------

type dockerConfigFile struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

type dockerAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

var errCredHelper = errors.New("credential helper not supported")

func dockerConfigPath() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".docker", "config.json")
	}
	return ""
}

// RegistryLogin holds credentials pushed from the hub's Settings -> Connections.
type RegistryLogin struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

var (
	managedRegMu   sync.RWMutex
	managedRegs    []RegistryLogin
	managedCfgPath string
)

// SetManagedRegistries updates in-memory registry logins for image update checks and
// writes a merged config.json under dataDir for docker compose pull when DOCKER_CONFIG
// wasn't explicitly set in the process environment.
func (e *Engine) SetManagedRegistries(list []RegistryLogin) error {
	managedRegMu.Lock()
	managedRegs = append([]RegistryLogin(nil), list...)
	managedRegMu.Unlock()

	if e == nil || e.cfg.DataDir == "" {
		return nil
	}
	dir := filepath.Join(e.cfg.DataDir, "docker-config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	auths := map[string]dockerAuthEntry{}
	for _, r := range list {
		if r.Server == "" || (r.Username == "" && r.Password == "") {
			continue
		}
		key := r.Server
		if key == "docker.io" {
			key = "https://index.docker.io/v1/"
		}
		auths[key] = dockerAuthEntry{
			Username: r.Username,
			Password: r.Password,
			Auth:     base64.StdEncoding.EncodeToString([]byte(r.Username + ":" + r.Password)),
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	b, err := json.MarshalIndent(dockerConfigFile{Auths: auths}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return err
	}
	managedRegMu.Lock()
	managedCfgPath = cfgPath
	managedRegMu.Unlock()
	if os.Getenv("DOCKER_CONFIG") == "" {
		_ = os.Setenv("DOCKER_CONFIG", dir)
	}
	return nil
}

func readRegistryAuth(ref string) (RegistryCreds, error) {
	regHost, _, _ := ParseImageRef(ref)
	managedRegMu.RLock()
	regs := managedRegs
	fallbackCfg := managedCfgPath
	managedRegMu.RUnlock()
	for _, r := range regs {
		if registryHostMatches(r.Server, regHost) && (r.Username != "" || r.Password != "") {
			srv := r.Server
			if srv == "docker.io" {
				srv = "https://index.docker.io/v1/"
			}
			payload, _ := json.Marshal(map[string]string{
				"username":      r.Username,
				"password":      r.Password,
				"serveraddress": srv,
			})
			return RegistryCreds{
				Username:    r.Username,
				Password:    r.Password,
				EncodedAuth: base64.URLEncoding.EncodeToString(payload),
			}, nil
		}
	}
	p := dockerConfigPath()
	if p == "" {
		p = fallbackCfg
	}
	return readRegistryAuthFromFile(p, ref)
}

func readRegistryAuthFromFile(path, ref string) (RegistryCreds, error) {
	regHost, _, _ := ParseImageRef(ref)
	if path == "" {
		return RegistryCreds{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return RegistryCreds{}, nil
		}
		return RegistryCreds{}, err
	}
	var cfg dockerConfigFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return RegistryCreds{}, nil
	}

	// Check if a credential helper is configured for this registry.
	for k, helper := range cfg.CredHelpers {
		if helper != "" && registryHostMatches(k, regHost) {
			return RegistryCreds{}, errCredHelper
		}
	}

	// Look for a matching auths entry.
	var matched *dockerAuthEntry
	var matchedKey string
	for k, entry := range cfg.Auths {
		if registryHostMatches(k, regHost) {
			e := entry
			matched = &e
			matchedKey = k
			break
		}
	}

	if matched != nil {
		user, pass := matched.Username, matched.Password
		if user == "" && pass == "" && matched.Auth != "" {
			if dec, err := base64.StdEncoding.DecodeString(matched.Auth); err == nil {
				if u, p, ok := strings.Cut(string(dec), ":"); ok {
					user, pass = u, p
				}
			}
		}
		if user != "" || pass != "" {
			payload, _ := json.Marshal(map[string]string{
				"username":      user,
				"password":      pass,
				"serveraddress": matchedKey,
			})
			return RegistryCreds{
				Username:      user,
				Password:      pass,
				ServerAddress: matchedKey,
				EncodedAuth:   base64.URLEncoding.EncodeToString(payload),
			}, nil
		}
		// Empty auth entry in auths combined with global credsStore means credentials live in an external helper.
		if cfg.CredsStore != "" {
			return RegistryCreds{}, errCredHelper
		}
	}
	// No entry for this registry: docker itself would pull anonymously, even with a global
	// credsStore (Docker Desktop always sets one), so public images on ghcr.io etc. still work.
	return RegistryCreds{}, nil
}

func normalizeRegistryKey(k string) string {
	k = strings.TrimSpace(strings.ToLower(k))
	k = strings.TrimPrefix(k, "https://")
	k = strings.TrimPrefix(k, "http://")
	if idx := strings.Index(k, "/"); idx >= 0 {
		k = k[:idx]
	}
	return k
}

func registryHostMatches(configKey, targetHost string) bool {
	nk := normalizeRegistryKey(configKey)
	nt := normalizeRegistryKey(targetHost)
	if nk == nt {
		return true
	}
	if isDockerHubHost(nk) && isDockerHubHost(nt) {
		return true
	}
	return false
}

func isDockerHubHost(h string) bool {
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

func classifyRegistryError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errCredHelper) {
		return "credential helper not supported"
	}
	var ngErr *netguard.Error
	if errors.As(err, &ngErr) {
		return "blocked address"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "credential helper not supported"):
		return "credential helper not supported"
	case strings.Contains(msg, "blocked "):
		return "blocked address"
	case strings.Contains(msg, "toomanyrequests") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "rate limit") || strings.Contains(msg, "429"):
		return "rate limited"
	case strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication required") || strings.Contains(msg, "access denied") || strings.Contains(msg, "denied") || strings.Contains(msg, "401") || strings.Contains(msg, "403"):
		return "unauthorized"
	case strings.Contains(msg, "manifest unknown") || strings.Contains(msg, "name unknown") || strings.Contains(msg, "not found") || strings.Contains(msg, "404"):
		return "not found"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	default:
		return "registry unreachable"
	}
}

// ---- Part B: Version parsing & candidate selection -------------------------

// VersionTag represents a strict numeric version tag like v1.27.3-alpine.
type VersionTag struct {
	Raw    string
	Prefix string // "" or "v"
	Parts  []int  // 1 to 3 numbers
	Suffix string // "" or "-[a-z0-9.]+"
}

var (
	versionTagRe = regexp.MustCompile(`^(v?)(\d+(?:\.\d+){0,2})(-[a-z0-9.]+)?$`)
	datePartRe   = regexp.MustCompile(`^\d{8}$`)
	preReleaseRe = regexp.MustCompile(`(?i)(?:^|[.-])(rc|alpha|beta|dev|pre|preview|nightly|canary|snapshot|test)\d*(?:$|[.-])`)
)

// ParseVersionTag parses tag if it is a strict 1..3-part numeric version tag, rejecting rolling/date tags.
func ParseVersionTag(tag string) (VersionTag, bool) {
	m := versionTagRe.FindStringSubmatch(tag)
	if m == nil {
		return VersionTag{}, false
	}
	numStr := m[2]
	rawParts := strings.Split(numStr, ".")
	if len(rawParts) == 1 && datePartRe.MatchString(rawParts[0]) {
		return VersionTag{}, false
	}
	parts := make([]int, len(rawParts))
	for i, p := range rawParts {
		if len(p) > 1 && p[0] == '0' {
			return VersionTag{}, false
		}
		if datePartRe.MatchString(p) {
			return VersionTag{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return VersionTag{}, false
		}
		parts[i] = n
	}
	return VersionTag{
		Raw:    tag,
		Prefix: m[1],
		Parts:  parts,
		Suffix: m[3],
	}, true
}

func isPreReleaseSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	return preReleaseRe.MatchString(suffix)
}

func compareVersionParts(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] > b[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// TagCandidateUpdater processes remote tags one by one in constant memory,
// keeping only the highest same-major and newer-major candidate for currentTag.
type TagCandidateUpdater struct {
	cur       VersionTag
	ok        bool
	bestSame  *VersionTag
	bestMajor *VersionTag
}

// NewTagCandidateUpdater initializes a streaming candidate selector for currentTag.
func NewTagCandidateUpdater(currentTag string) *TagCandidateUpdater {
	vt, ok := ParseVersionTag(currentTag)
	if !ok || isPreReleaseSuffix(vt.Suffix) {
		return &TagCandidateUpdater{ok: false}
	}
	return &TagCandidateUpdater{cur: vt, ok: true}
}

// Consider checks one candidate tag string.
func (u *TagCandidateUpdater) Consider(tag string) {
	if !u.ok {
		return
	}
	cand, ok := ParseVersionTag(tag)
	if !ok {
		return
	}
	if cand.Prefix != u.cur.Prefix || len(cand.Parts) != len(u.cur.Parts) || cand.Suffix != u.cur.Suffix {
		return
	}
	if compareVersionParts(cand.Parts, u.cur.Parts) <= 0 {
		return
	}
	if cand.Parts[0] == u.cur.Parts[0] {
		if u.bestSame == nil || compareVersionParts(cand.Parts, u.bestSame.Parts) > 0 {
			c := cand
			u.bestSame = &c
		}
	} else if cand.Parts[0] > u.cur.Parts[0] {
		if u.bestMajor == nil || compareVersionParts(cand.Parts, u.bestMajor.Parts) > 0 {
			c := cand
			u.bestMajor = &c
		}
	}
}

// Result returns the best same-major and newer-major tag names found.
func (u *TagCandidateUpdater) Result() (sameMajor, newerMajor string) {
	if u.bestSame != nil {
		sameMajor = u.bestSame.Raw
	}
	if u.bestMajor != nil {
		newerMajor = u.bestMajor.Raw
	}
	return sameMajor, newerMajor
}

// SelectNewerTags returns the highest same-major and newer-major candidates for currentTag from tags.
func SelectNewerTags(currentTag string, tags []string) (sameMajor, newerMajor string) {
	u := NewTagCandidateUpdater(currentTag)
	for _, t := range tags {
		u.Consider(t)
	}
	return u.Result()
}

// ---- Registry HTTP API v2 tag lister ---------------------------------------

// ParseImageRef splits an image reference into registry host, repository path, and tag.
func ParseImageRef(ref string) (registry, repo, tag string) {
	ref = strings.TrimSpace(ref)
	if idx := strings.Index(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}
	tag = "latest"
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		tag = ref[lastColon+1:]
		ref = ref[:lastColon]
	}
	first, rest, hasSlash := strings.Cut(ref, "/")
	if !hasSlash {
		return "registry-1.docker.io", "library/" + first, tag
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		if isDockerHubHost(first) {
			if !strings.Contains(rest, "/") {
				rest = "library/" + rest
			}
			return "registry-1.docker.io", rest, tag
		}
		return first, rest, tag
	}
	return "registry-1.docker.io", ref, tag
}

var registryHTTPClient = &http.Client{
	Timeout: imageReqTimeout,
	Transport: &http.Transport{
		DialContext:     netguard.DialContext(10*time.Second, false),
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// SelectNewerTagsFromRegistry lists tags via Registry HTTP API v2 (streaming JSON, pagination up to 10,000 tags,
// Bearer token auth, and netguard SSRF protection) and returns the best same-major and newer-major tag.
// If clientOverride is nil, the netguard-protected registryHTTPClient is used.
func SelectNewerTagsFromRegistry(ctx context.Context, ref, currentTag string, creds RegistryCreds, clientOverride *http.Client) (string, string, error) {
	upd := NewTagCandidateUpdater(currentTag)
	if !upd.ok {
		return "", "", nil
	}
	cl := registryHTTPClient
	if clientOverride != nil {
		cl = clientOverride
	}
	regHost, repo, _ := ParseImageRef(ref)
	scheme := "https"
	baseURL := fmt.Sprintf("%s://%s", scheme, regHost)
	nextPath := fmt.Sprintf("/v2/%s/tags/list?n=1000", repo)

	bearerToken := ""
	totalSeen := 0

	for nextPath != "" && totalSeen < maxRemoteTags {
		fullURL := resolveRegistryURL(baseURL, nextPath)
		resp, err := doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
		if err != nil && scheme == "https" && isLocalRegistryHost(regHost) {
			scheme = "http"
			baseURL = fmt.Sprintf("http://%s", regHost)
			fullURL = resolveRegistryURL(baseURL, nextPath)
			resp, err = doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
		}
		if err != nil {
			return "", "", err
		}

		if resp.StatusCode == http.StatusUnauthorized && bearerToken == "" {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, terr := fetchBearerToken(ctx, cl, challenge, creds)
			if terr != nil {
				return "", "", terr
			}
			bearerToken = tok
			resp, err = doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
			if err != nil {
				return "", "", err
			}
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return "", "", fmt.Errorf("registry answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		linkNext := parseNextLink(resp.Header.Get("Link"))
		seenInPage, perr := streamTagsResponse(resp.Body, maxRemoteTags-totalSeen, upd.Consider)
		resp.Body.Close()
		if perr != nil {
			return "", "", perr
		}
		totalSeen += seenInPage
		if seenInPage == 0 || linkNext == "" || linkNext == nextPath {
			break
		}
		nextPath = linkNext
	}

	same, major := upd.Result()
	return same, major, nil
}

// LatestStableTagFromRegistry queries the registry HTTP API v2 tag list for ref and returns
// the highest stable semantic version tag (preferring non-prerelease tags).
func LatestStableTagFromRegistry(ctx context.Context, ref string, creds RegistryCreds, clientOverride *http.Client) (string, error) {
	cl := registryHTTPClient
	if clientOverride != nil {
		cl = clientOverride
	}
	regHost, repo, curTag := ParseImageRef(ref)
	wantSuffix := ""
	if idx := strings.Index(curTag, "-"); idx > 0 && curTag != "latest" {
		wantSuffix = curTag[idx:]
	}
	scheme := "https"
	baseURL := fmt.Sprintf("%s://%s", scheme, regHost)
	nextPath := fmt.Sprintf("/v2/%s/tags/list?n=1000", repo)

	bearerToken := ""
	totalSeen := 0
	var best *VersionTag

	consider := func(tag string) {
		vt, ok := ParseVersionTag(tag)
		if !ok || isPreReleaseSuffix(vt.Suffix) {
			return
		}
		if wantSuffix != "" && vt.Suffix != wantSuffix {
			return
		}
		if wantSuffix == "" && vt.Suffix != "" {
			return
		}
		if best == nil || compareVersionSlices(vt.Parts, best.Parts) > 0 {
			c := vt
			best = &c
		}
	}

	for nextPath != "" && totalSeen < maxRemoteTags {
		fullURL := resolveRegistryURL(baseURL, nextPath)
		resp, err := doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
		if err != nil && scheme == "https" && isLocalRegistryHost(regHost) {
			scheme = "http"
			baseURL = fmt.Sprintf("http://%s", regHost)
			fullURL = resolveRegistryURL(baseURL, nextPath)
			resp, err = doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
		}
		if err != nil {
			return "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && bearerToken == "" {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			tok, terr := fetchBearerToken(ctx, cl, challenge, creds)
			if terr != nil {
				return "", terr
			}
			bearerToken = tok
			resp, err = doRegistryGet(ctx, cl, fullURL, creds, bearerToken)
			if err != nil {
				return "", err
			}
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return "", fmt.Errorf("registry answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		linkNext := parseNextLink(resp.Header.Get("Link"))
		seenInPage, perr := streamTagsResponse(resp.Body, maxRemoteTags-totalSeen, consider)
		resp.Body.Close()
		if perr != nil {
			return "", perr
		}
		totalSeen += seenInPage
		if seenInPage == 0 || linkNext == "" || linkNext == nextPath {
			break
		}
		nextPath = linkNext
	}
	if best == nil {
		return "", errors.New("no stable version tag found in registry")
	}
	return best.Raw, nil
}

func compareVersionSlices(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

// manifestAccept lists the manifest types Moorage understands, index types first: the
// digest of a multi-platform image is its index digest, the same DistributionInspect
// returns and the one a pull records in RepoDigests.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// RemoteDigestFromRegistry asks the registry for the digest of ref's manifest with a HEAD
// request (Registry HTTP API v2; Docker Hub doesn't count HEAD against the pull limit).
// Used where the engine can't answer DistributionInspect (Podman). If clientOverride is
// nil, the netguard-protected registryHTTPClient is used.
func RemoteDigestFromRegistry(ctx context.Context, ref string, creds RegistryCreds, clientOverride *http.Client) (string, error) {
	cl := registryHTTPClient
	if clientOverride != nil {
		cl = clientOverride
	}
	regHost, repo, tag := ParseImageRef(ref)
	path := fmt.Sprintf("/v2/%s/manifests/%s", repo, tag)
	head := func(base, bearer string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+path, nil)
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
	resp, err := head(base, "")
	if err != nil && isLocalRegistryHost(regHost) {
		base = "http://" + regHost
		resp, err = head(base, "")
	}
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		tok, terr := fetchBearerToken(ctx, cl, challenge, creds)
		if terr != nil {
			return "", terr
		}
		if resp, err = head(base, tok); err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry answered %d for the manifest", resp.StatusCode)
	}
	d := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest"))
	if !digestRe.MatchString(d) {
		return "", errors.New("registry sent no usable manifest digest")
	}
	return d, nil
}

func isLocalRegistryHost(h string) bool {
	return strings.HasPrefix(h, "127.0.0.1:") || strings.HasPrefix(h, "localhost:") || h == "127.0.0.1" || h == "localhost"
}

func resolveRegistryURL(baseURL, pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "https://") || strings.HasPrefix(pathOrURL, "http://") {
		if u, err := url.Parse(pathOrURL); err == nil {
			if bu, err := url.Parse(baseURL); err == nil && u.Host == bu.Host {
				return baseURL + u.RequestURI()
			}
		}
	}
	if !strings.HasPrefix(pathOrURL, "/") {
		pathOrURL = "/" + pathOrURL
	}
	return baseURL + pathOrURL
}

func doRegistryGet(ctx context.Context, cl *http.Client, u string, creds RegistryCreds, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Moorage")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if creds.Username != "" || creds.Password != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	return cl.Do(req)
}

var authParamRe = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

func fetchBearerToken(ctx context.Context, cl *http.Client, header string, creds RegistryCreds) (string, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return "", errors.New("unauthorized")
	}
	params := map[string]string{}
	for _, m := range authParamRe.FindAllStringSubmatch(header, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("unauthorized: missing bearer realm")
	}
	u, err := url.Parse(realm)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("unauthorized: invalid bearer realm")
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if sc := params["scope"]; sc != "" {
		q.Set("scope", sc)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Moorage")
	if creds.Username != "" || creds.Password != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unauthorized (%d)", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&tok); err != nil {
		return "", err
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", errors.New("unauthorized: empty token")
}

func parseNextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		if !strings.Contains(part, `rel="next"`) && !strings.Contains(part, `rel=next`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start >= 0 && end > start+1 {
			return part[start+1 : end]
		}
	}
	return ""
}

// streamTagsResponse reads {"tags":[...]} token-by-token so 10,000 tags never allocate a large slice.
func streamTagsResponse(r io.Reader, limit int, fn func(tag string)) (int, error) {
	dec := json.NewDecoder(io.LimitReader(r, 8<<20))
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		key, ok := tok.(string)
		if !ok || key != "tags" {
			continue
		}
		// Next token may be '[' or nil (null).
		next, err := dec.Token()
		if err != nil {
			return count, err
		}
		if next == nil {
			return count, nil
		}
		if delim, ok := next.(json.Delim); !ok || delim != '[' {
			continue
		}
		for dec.More() {
			var tag string
			if err := dec.Decode(&tag); err != nil {
				return count, err
			}
			if count < limit {
				fn(tag)
				count++
			}
		}
		return count, nil
	}
}

// ---- Part B: Targeted compose.yaml rewrite ---------------------------------

// RewriteComposeImage performs a targeted text edit on compose.yaml to change the image of targetService
// from oldRef to newRef, preserving quotes, comments, indentation, and line endings (CRLF vs LF).
// If the value uses a variable (${TAG}), does not match oldRef, or oldRef appears more than once as an image
// value in the file, it returns ErrManualEdit ("Edit compose.yaml manually").
func RewriteComposeImage(composeText, targetService, oldRef, newRef string) (string, error) {
	if targetService == "" || oldRef == "" || newRef == "" || oldRef == newRef {
		return "", ErrManualEdit
	}
	crlf := strings.Contains(composeText, "\r\n")
	normalized := strings.ReplaceAll(composeText, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")

	inServices := false
	servicesIndent := -1
	currentService := ""
	serviceIndent := -1

	targetLineIdx := -1
	var rewrittenTargetLine string
	oldRefCount := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(line)
		if strings.Contains(line[:indent], "\t") {
			return "", ErrManualEdit
		}

		// Top-level or section check.
		if !inServices || indent <= servicesIndent {
			inServices = false
			currentService = ""
			serviceIndent = -1
			if key, rest, ok := parseYAMLKeyLine(trimmed); ok && key == "services" && rest == "" {
				inServices = true
				servicesIndent = indent
			}
			continue
		}

		// Inside services: block.
		if serviceIndent == -1 || indent == serviceIndent {
			if key, rest, ok := parseYAMLKeyLine(trimmed); ok && rest == "" {
				currentService = key
				serviceIndent = indent
				continue
			}
		}
		if serviceIndent != -1 && indent < serviceIndent {
			currentService = ""
			continue
		}

		// Property inside currentService.
		if currentService != "" && indent > serviceIndent {
			key, _, ok := parseYAMLKeyLine(trimmed)
			if !ok || key != "image" {
				continue
			}
			val, newLine, hasVar, ok := parseAndReplaceImageLine(line, newRef)
			if !ok {
				if currentService == targetService {
					return "", ErrManualEdit
				}
				continue
			}
			if val == oldRef {
				oldRefCount++
			}
			if currentService == targetService {
				if hasVar || val != oldRef || targetLineIdx != -1 {
					return "", ErrManualEdit
				}
				targetLineIdx = i
				rewrittenTargetLine = newLine
			}
		}
	}

	if targetLineIdx == -1 || oldRefCount != 1 {
		return "", ErrManualEdit
	}
	lines[targetLineIdx] = rewrittenTargetLine
	sep := "\n"
	if crlf {
		sep = "\r\n"
	}
	return strings.Join(lines, sep), nil
}

func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

func parseYAMLKeyLine(trimmed string) (key, rest string, ok bool) {
	idx := strings.IndexByte(trimmed, ':')
	if idx <= 0 {
		return "", "", false
	}
	key = trimmed[:idx]
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return "", "", false
		}
	}
	after := trimmed[idx+1:]
	if after != "" && after[0] != ' ' && after[0] != '\t' && after[0] != '#' {
		return "", "", false
	}
	after = strings.TrimSpace(after)
	if strings.HasPrefix(after, "#") {
		after = ""
	}
	return key, after, true
}

func parseAndReplaceImageLine(line, newRef string) (val, replaced string, hasVar, ok bool) {
	colonIdx := strings.Index(line, "image:")
	if colonIdx < 0 {
		return "", "", false, false
	}
	prefix := line[:colonIdx+len("image:")]
	rest := line[colonIdx+len("image:"):]

	spaceLen := 0
	for spaceLen < len(rest) && (rest[spaceLen] == ' ' || rest[spaceLen] == '\t') {
		spaceLen++
	}
	if spaceLen == 0 {
		return "", "", false, false
	}
	prefix += rest[:spaceLen]
	rest = rest[spaceLen:]
	if rest == "" {
		return "", "", false, false
	}

	var rawVal, suffix string
	var quote byte
	if rest[0] == '"' || rest[0] == '\'' {
		quote = rest[0]
		endIdx := strings.IndexByte(rest[1:], quote)
		if endIdx < 0 {
			return "", "", false, false
		}
		rawVal = rest[1 : 1+endIdx]
		suffix = rest[1+endIdx+1:]
		if strings.TrimSpace(suffix) != "" && !strings.HasPrefix(strings.TrimSpace(suffix), "#") {
			return "", "", false, false
		}
	} else {
		// Unquoted value up to whitespace or inline comment ' #'.
		end := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == ' ' || rest[i] == '\t' {
				end = i
				break
			}
		}
		rawVal = rest[:end]
		suffix = rest[end:]
		if strings.TrimSpace(suffix) != "" && !strings.HasPrefix(strings.TrimSpace(suffix), "#") {
			return "", "", false, false
		}
	}

	if strings.Contains(rawVal, "$") {
		return rawVal, "", true, true
	}
	var newValStr string
	if quote != 0 {
		newValStr = string(quote) + newRef + string(quote)
	} else {
		newValStr = newRef
	}
	return rawVal, prefix + newValStr + suffix, false, true
}

// ChangeServiceImage rewrites the service's image tag in compose.yaml, validates it, saves it atomically,
// and commits a git version with message "Change <service> image <repo> <oldTag> → <newTag>".
func (e *Engine) ChangeServiceImage(ctx context.Context, stack, service, oldRef, newRef, author string) (*Version, error) {
	dir, err := e.stackDir(stack)
	if err != nil {
		return nil, err
	}
	cf := composeFile(dir)
	if cf == "" {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, ErrReadOnly
	}
	if !e.lock(stack) {
		return nil, ErrBusy
	}
	defer e.unlock(stack)

	composeBytes, err := os.ReadFile(cf)
	if err != nil {
		return nil, err
	}
	envText := ""
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		envText = string(b)
	}

	rewritten, err := RewriteComposeImage(string(composeBytes), service, oldRef, newRef)
	if err != nil {
		return nil, err
	}
	if err := e.ValidateCompose(ctx, stack, rewritten, envText); err != nil {
		return nil, err
	}
	e.syncExternal(stack)
	if err := e.writeFile(cf, rewritten); err != nil {
		return nil, err
	}
	msg := FormatImageChangeMessage(service, oldRef, newRef)
	v := e.record(stack, author, KindEdit, "", func([]string) string { return msg })
	return v, nil
}

// FormatImageChangeMessage formats the commit message for a tag update:
// "Change web image traefik/whoami v1.10 → v1.11".
func FormatImageChangeMessage(service, oldRef, newRef string) string {
	repo, oldTag := splitRefDisplay(oldRef)
	_, newTag := splitRefDisplay(newRef)
	return fmt.Sprintf("Change %s image %s %s → %s", service, repo, oldTag, newTag)
}

func splitRefDisplay(ref string) (repo, tag string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, "latest"
}
