// Package imagefind turns an image name into compose file candidates for "New stack":
// a matching template, the compose file of the image's source repository, and one
// generated from the image's own metadata. Everything it returns is shown for review
// before a stack is created; nothing is run.
package imagefind

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"

	"moorage/internal/netguard"
)

const (
	maxBody       = 1 << 20 // per response
	lookupTimeout = 20 * time.Second
	cacheFor      = time.Hour
)

// Candidate is one way to start the stack.
type Candidate struct {
	Source   string   `json:"source"` // template, repo, generated
	Title    string   `json:"title"`
	Detail   string   `json:"detail,omitempty"`
	URL      string   `json:"url,omitempty"` // where it came from
	Compose  string   `json:"compose,omitempty"`
	Env      string   `json:"env,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// TemplateSource/TemplateID open the template's own deploy flow (variables, secrets).
	TemplateSource string `json:"templateSource,omitempty"`
	TemplateID     string `json:"templateId,omitempty"`
}

// Result is the answer for one image.
type Result struct {
	Image      string      `json:"image"` // normalised, with tag
	Name       string      `json:"name"`  // suggested stack name
	Candidates []Candidate `json:"candidates"`
	// Problems are lookups that failed (registry unreachable, private image…), as text.
	Problems []string `json:"problems,omitempty"`
}

// TemplateMatch is a template whose compose file uses the image.
type TemplateMatch struct {
	Source, ID, Name, Description, SourceLabel string
}

// Finder looks images up. The zero value works against the real internet.
type Finder struct {
	// Templates returns templates whose compose uses the repository (e.g. "library/postgres").
	Templates func(ctx context.Context, repo string) []TemplateMatch
	// Client overrides the HTTP client (tests).
	Client *http.Client
	// GitHosts maps the public code hosts to their API/raw base URLs (tests point them
	// at a local server).
	GitHosts map[string]GitHost

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at  time.Time
	res Result
}

// GitHost knows how to read a repository's default branch and a raw file.
type GitHost struct {
	DefaultBranch func(owner, repo string) string // API URL returning JSON with default_branch
	Raw           func(owner, repo, branch, file string) string
}

var defaultGitHosts = map[string]GitHost{
	"github.com": {
		DefaultBranch: func(o, r string) string { return "https://api.github.com/repos/" + o + "/" + r },
		Raw: func(o, r, b, f string) string {
			return "https://raw.githubusercontent.com/" + o + "/" + r + "/" + url.PathEscape(b) + "/" + f
		},
	},
	"gitlab.com": {
		DefaultBranch: func(o, r string) string { return "https://gitlab.com/api/v4/projects/" + url.PathEscape(o+"/"+r) },
		Raw: func(o, r, b, f string) string {
			return "https://gitlab.com/" + o + "/" + r + "/-/raw/" + url.PathEscape(b) + "/" + f
		},
	},
	"codeberg.org": {
		DefaultBranch: func(o, r string) string { return "https://codeberg.org/api/v1/repos/" + o + "/" + r },
		Raw: func(o, r, b, f string) string {
			return "https://codeberg.org/" + o + "/" + r + "/raw/branch/" + url.PathEscape(b) + "/" + f
		},
	},
}

// composePaths are tried in each repository, in this order.
var composePaths = []string{
	"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
	"docker/compose.yaml", "docker/docker-compose.yml", "deploy/compose.yaml", "deploy/docker-compose.yml",
}

// ErrBadImage is an image name that can't be parsed.
var ErrBadImage = errors.New("that isn't an image name, e.g. nginx, ghcr.io/owner/app:1.2")

// Lookup finds candidates for image. Failures of single sources become Problems; the
// generated candidate needs only the registry.
func (f *Finder) Lookup(ctx context.Context, image string) (Result, error) {
	image = strings.TrimSpace(image)
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil || len(image) > 255 {
		return Result{}, ErrBadImage
	}
	named = reference.TagNameOnly(named)
	full := reference.FamiliarString(named)
	if r, ok := f.fromCache(full); ok {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	res := Result{Image: full, Name: stackName(reference.Path(named)), Candidates: []Candidate{}}
	repo := reference.Path(named)
	if f.Templates != nil {
		for _, t := range f.Templates(ctx, repo) {
			res.Candidates = append(res.Candidates, Candidate{
				Source: "template", Title: t.Name, Detail: t.Description + " (" + t.SourceLabel + ")",
				TemplateSource: t.Source, TemplateID: t.ID,
			})
		}
	}

	cfg, err := f.imageConfig(ctx, named)
	if err != nil {
		res.Problems = append(res.Problems, "Registry: "+err.Error())
	} else {
		if src := cfg.Config.Labels["org.opencontainers.image.source"]; src != "" {
			c, prob := f.repoCompose(ctx, src, full)
			if prob != "" {
				res.Problems = append(res.Problems, prob)
			} else if c != nil {
				res.Candidates = append(res.Candidates, *c)
			}
		}
		res.Candidates = append(res.Candidates, generate(res.Name, full, cfg))
	}
	if len(res.Candidates) == 0 && len(res.Problems) == 0 {
		res.Problems = append(res.Problems, "Nothing found for this image.")
	}
	f.toCache(full, res)
	return res, nil
}

func (f *Finder) fromCache(k string) (Result, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cache[k]
	if !ok || time.Since(c.at) > cacheFor {
		return Result{}, false
	}
	return c.res, true
}

func (f *Finder) toCache(k string, r Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]cached{}
	}
	if len(f.cache) > 200 { // bounded: drop everything rather than track ages
		f.cache = map[string]cached{}
	}
	f.cache[k] = cached{at: time.Now(), res: r}
}

var generic = map[string]bool{"server": true, "web": true, "docker": true, "image": true, "core": true, "api": true}

var nonName = regexp.MustCompile(`[^a-z0-9_-]+`)

// stackName suggests a stack name from the repository's last path part.
func stackName(path string) string {
	n := path[strings.LastIndex(path, "/")+1:]
	// "vaultwarden/server" is better named after its owner than "server".
	if generic[n] && strings.Contains(path, "/") {
		parts := strings.Split(path, "/")
		n = parts[len(parts)-2]
	}
	n = strings.Trim(nonName.ReplaceAllString(strings.ToLower(n), "-"), "-_")
	if n == "" {
		n = "app"
	}
	if len(n) > 40 {
		n = n[:40]
	}
	return n
}

// client is HTTPS-capable, checks every dialled address against the metadata and
// link-local guard, and allows private addresses: a homelab registry is legitimate,
// and the code hosts are fixed public names.
func (f *Finder) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           netguard.DialContext(10*time.Second, false),
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   lookupTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a non-https URL refused")
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func (f *Finder) get(ctx context.Context, u string, hdr http.Header) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "Moorage image lookup")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, nil, friendly(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return resp, nil, err
	}
	if len(b) > maxBody {
		return resp, nil, errors.New("response larger than 1 MiB")
	}
	return resp, b, nil
}

func friendly(err error) error {
	var ne *netguard.Error
	if errors.As(err, &ne) {
		return ne
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return errors.New("timed out")
		}
		return ue.Err
	}
	return err
}

// jsonField reads one string field of a JSON object (default_branch).
func jsonField(b []byte, field string) string {
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
