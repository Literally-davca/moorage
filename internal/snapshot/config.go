// Package snapshot publishes static copies of the public status pages (the
// status-snapshot add-on, M18D) to storage the admin already has: an S3-compatible
// bucket (AWS S3, Cloudflare R2, MinIO, Garage…), a WebDAV folder or a GitHub Pages
// branch. Moorage never creates the bucket, folder or repository: it only writes files
// into a target that exists, and the Test button checks that it can.
//
// Credentials live in the settings table like the other integration secrets, are never
// returned by the API (only "set" flags) and never appear in logs or error messages.
package snapshot

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Providers.
const (
	ProviderS3     = "s3"
	ProviderWebDAV = "webdav"
	ProviderGitHub = "github"
)

// Defaults and limits.
const (
	DefaultInterval = 5  // minutes between periodic publishes
	DefaultStale    = 15 // minutes after which a snapshot says it may be out of date
	MinInterval     = 1
	MaxInterval     = 60
	MaxStale        = 1440
	// GitHub Pages rebuilds the site on every push and allows about 10 builds an hour,
	// and its CDN caches pages for up to 10 minutes.
	MinGitHubInterval = 15
	MinGitHubStale    = 30
)

// Config is the saved add-on configuration (JSON in settings).
type Config struct {
	Provider string  `json:"provider"`
	Pages    []int64 `json:"pages,omitempty"` // empty = every published page
	Interval int     `json:"intervalMinutes"`
	Stale    int     `json:"staleMinutes"`

	S3Endpoint  string `json:"s3Endpoint,omitempty"`
	S3Bucket    string `json:"s3Bucket,omitempty"`
	S3Region    string `json:"s3Region,omitempty"`
	S3AccessKey string `json:"s3AccessKey,omitempty"`
	S3SecretKey string `json:"s3SecretKey,omitempty"`
	S3Prefix    string `json:"s3Prefix,omitempty"`

	WebDAVURL      string `json:"webdavUrl,omitempty"`
	WebDAVUsername string `json:"webdavUsername,omitempty"`
	WebDAVPassword string `json:"webdavPassword,omitempty"`

	GitHubRepo   string `json:"githubRepo,omitempty"`   // owner/name
	GitHubBranch string `json:"githubBranch,omitempty"` // default gh-pages
	GitHubPath   string `json:"githubPath,omitempty"`   // folder in the branch, empty = root
	GitHubToken  string `json:"githubToken,omitempty"`
}

// IntervalDuration is the periodic publish interval.
func (c Config) IntervalDuration() time.Duration {
	return time.Duration(c.Interval) * time.Minute
}

// StaleDuration is how old a snapshot may get before it says it may be out of date.
func (c Config) StaleDuration() time.Duration {
	return time.Duration(c.Stale) * time.Minute
}

// MinGap is the shortest time between two publishes (GitHub Pages build limit).
func (c Config) MinGap() time.Duration {
	if c.Provider == ProviderGitHub {
		return 6 * time.Minute
	}
	return 30 * time.Second
}

// Configured reports whether a provider is chosen.
func (c Config) Configured() bool {
	return c.Provider == ProviderS3 || c.Provider == ProviderWebDAV || c.Provider == ProviderGitHub
}

var (
	safePath   = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
	branchRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	bucketRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRe   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	errNoPages = errors.New("No published status page to publish (publish a page under Status pages first)")
)

// cleanFolder normalises a folder prefix to "" or "a/b/" and refuses anything that could
// leave the target folder.
func cleanFolder(p, what string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if len(p) > 200 || !safePath.MatchString(p) {
		return "", fmt.Errorf("The %s may contain only letters, digits, '.', '_', '-' and '/'.", what)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("The %s isn't a valid folder.", what)
		}
	}
	return p + "/", nil
}

func checkURL(raw, what string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("The %s must be an http:// or https:// URL.", what)
	}
	if u.User != nil {
		return "", fmt.Errorf("Put the credentials into their own fields, not into the %s.", what)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("The %s can't have a query or fragment.", what)
	}
	return raw, nil
}

// Normalize fills defaults and validates c. Secrets are only checked for presence.
func (c *Config) Normalize() error {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.Stale == 0 {
		c.Stale = DefaultStale
	}
	if c.Provider == ProviderGitHub {
		c.Interval = max(c.Interval, MinGitHubInterval)
		if c.Stale < MinGitHubStale {
			c.Stale = MinGitHubStale
		}
	}
	if c.Interval < MinInterval || c.Interval > MaxInterval {
		return fmt.Errorf("Publish every %d–%d minutes.", MinInterval, MaxInterval)
	}
	if c.Stale <= c.Interval || c.Stale > MaxStale {
		return fmt.Errorf("The out-of-date notice must come after more than the publish interval (%d min) and at most after 24 hours.", c.Interval)
	}
	seen := map[int64]bool{}
	pages := c.Pages[:0]
	for _, id := range c.Pages {
		if id > 0 && !seen[id] {
			seen[id] = true
			pages = append(pages, id)
		}
	}
	c.Pages = pages
	if len(c.Pages) > 50 {
		return errors.New("Pick at most 50 status pages.")
	}
	var err error
	switch c.Provider {
	case ProviderS3:
		if c.S3Endpoint, err = checkURL(c.S3Endpoint, "S3 endpoint"); err != nil {
			return err
		}
		c.S3Bucket = strings.TrimSpace(c.S3Bucket)
		if !bucketRe.MatchString(c.S3Bucket) {
			return errors.New("Enter the name of an existing bucket (lowercase letters, digits, '.' and '-').")
		}
		c.S3Region = strings.TrimSpace(c.S3Region)
		if c.S3Region == "" {
			c.S3Region = "auto"
		}
		if !regionRe.MatchString(c.S3Region) {
			return errors.New("The region looks like us-east-1 or auto.")
		}
		c.S3AccessKey = strings.TrimSpace(c.S3AccessKey)
		if c.S3AccessKey == "" || c.S3SecretKey == "" {
			return errors.New("Enter the S3 access key and secret key.")
		}
		if c.S3Prefix, err = cleanFolder(c.S3Prefix, "folder"); err != nil {
			return err
		}
	case ProviderWebDAV:
		if c.WebDAVURL, err = checkURL(c.WebDAVURL, "WebDAV folder URL"); err != nil {
			return err
		}
		c.WebDAVUsername = strings.TrimSpace(c.WebDAVUsername)
	case ProviderGitHub:
		c.GitHubRepo = strings.TrimSpace(c.GitHubRepo)
		if !repoRe.MatchString(c.GitHubRepo) {
			return errors.New("Enter the repository as owner/name, for example acme/status.")
		}
		c.GitHubBranch = strings.TrimSpace(c.GitHubBranch)
		if c.GitHubBranch == "" {
			c.GitHubBranch = "gh-pages"
		}
		if !branchRe.MatchString(c.GitHubBranch) || strings.Contains(c.GitHubBranch, "..") || strings.HasPrefix(c.GitHubBranch, "/") || strings.HasSuffix(c.GitHubBranch, "/") {
			return errors.New("Enter a valid branch name, for example gh-pages.")
		}
		if c.GitHubPath, err = cleanFolder(c.GitHubPath, "folder"); err != nil {
			return err
		}
		if c.GitHubToken == "" {
			return errors.New("Enter a GitHub token that can write to the repository's contents.")
		}
	case "":
		return errors.New("Pick where to publish: S3, WebDAV or GitHub Pages.")
	default:
		return errors.New("Pick where to publish: S3, WebDAV or GitHub Pages.")
	}
	return nil
}

// Describe names the target for the UI and the audit log (never with credentials).
func (c Config) Describe() string {
	switch c.Provider {
	case ProviderS3:
		return "s3://" + c.S3Bucket + "/" + c.S3Prefix
	case ProviderWebDAV:
		return c.WebDAVURL + "/"
	case ProviderGitHub:
		return "github.com/" + c.GitHubRepo + "@" + c.GitHubBranch + "/" + c.GitHubPath
	}
	return ""
}

// Merge keeps saved secrets that the form left empty (the API never sends them back),
// but only for the same endpoint, folder URL or repository: a secret is never sent to
// another place than the one it was entered for.
func (c *Config) Merge(saved Config) {
	same := func(a, b string) bool {
		return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
	}
	if c.S3SecretKey == "" && same(c.S3Endpoint, saved.S3Endpoint) && strings.TrimSpace(c.S3AccessKey) == saved.S3AccessKey {
		c.S3SecretKey = saved.S3SecretKey
	}
	if c.WebDAVPassword == "" && same(c.WebDAVURL, saved.WebDAVURL) {
		c.WebDAVPassword = saved.WebDAVPassword
	}
	if c.GitHubToken == "" && strings.EqualFold(strings.TrimSpace(c.GitHubRepo), saved.GitHubRepo) {
		c.GitHubToken = saved.GitHubToken
	}
}
