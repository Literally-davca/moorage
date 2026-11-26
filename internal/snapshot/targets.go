package snapshot

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"moorage/internal/netguard"
)

// File is one file of a snapshot, its path relative to the target folder.
type File struct {
	Path        string
	ContentType string
	Data        []byte
}

// Target is where snapshots go.
type Target interface {
	// Publish writes the files, as one commit where the target has commits.
	Publish(ctx context.Context, files []File) error
	// Test checks write access and leaves nothing visible behind.
	Test(ctx context.Context) error
}

// testFile is written and removed again by Test.
const testFile = ".moorage-write-test"

// GitHubAPI is the GitHub REST API base URL (a var for tests).
var GitHubAPI = "https://api.github.com"

// NewTarget builds the target of a normalised config.
func NewTarget(c Config) (Target, error) {
	hc := client()
	switch c.Provider {
	case ProviderS3:
		return &s3Target{c: c, hc: hc}, nil
	case ProviderWebDAV:
		return &webdavTarget{c: c, hc: hc}, nil
	case ProviderGitHub:
		return &githubTarget{c: c, hc: hc, api: strings.TrimRight(GitHubAPI, "/")}, nil
	}
	return nil, errors.New("pick where to publish first")
}

// client refuses link-local and cloud metadata addresses (netguard) and never follows
// redirects, so credentials only ever go to the configured host.
func client() *http.Client {
	return &http.Client{
		Timeout:       60 * time.Second,
		Transport:     &http.Transport{DialContext: netguard.DialContext(10*time.Second, false), ForceAttemptHTTP2: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// httpError turns an unexpected answer into an error with the status and a short,
// sanitised reason from the body (S3 <Code>, GitHub "message"), never a credential.
func httpError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	reason := ""
	var gh struct {
		Message string `json:"message"`
	}
	var s3 struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	switch {
	case json.Unmarshal(body, &gh) == nil && gh.Message != "":
		reason = gh.Message
	case xml.Unmarshal(body, &s3) == nil && s3.Code != "":
		reason = s3.Code
		if s3.Message != "" {
			reason += ": " + s3.Message
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > 160 {
		reason = reason[:160] + "…"
	}
	msg := fmt.Sprintf("%s: HTTP %d", what, resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (check the credentials and their write permission)"
	case http.StatusNotFound:
		msg += " (the bucket, folder, repository or branch must already exist)"
	}
	if reason != "" {
		msg += " – " + reason
	}
	return errors.New(msg)
}

// doErr strips the URL from transport errors (it never holds credentials, but keeps
// the messages short) and keeps the cause.
func doErr(what string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// ---- S3 (path-style requests, AWS Signature V4: AWS S3, R2, MinIO, Garage) ----------

type s3Target struct {
	c  Config
	hc *http.Client
}

func (t *s3Target) key(p string) string { return t.c.S3Prefix + p }

func (t *s3Target) Publish(ctx context.Context, files []File) error {
	for _, f := range files {
		if err := t.do(ctx, http.MethodPut, t.key(f.Path), f.ContentType, f.Data); err != nil {
			return err
		}
	}
	return nil
}

func (t *s3Target) Test(ctx context.Context) error {
	if err := t.do(ctx, http.MethodPut, t.key(testFile), "text/plain; charset=utf-8", []byte("Moorage write test\n")); err != nil {
		return err
	}
	return t.do(ctx, http.MethodDelete, t.key(testFile), "", nil)
}

func (t *s3Target) do(ctx context.Context, method, key, contentType string, body []byte) error {
	base, err := url.Parse(t.c.S3Endpoint)
	if err != nil {
		return err
	}
	u := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: strings.TrimRight(base.Path, "/") + "/" + t.c.S3Bucket + "/" + key}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Cache-Control", "no-cache")
	}
	signS3(req, body, t.c.S3Region, t.c.S3AccessKey, t.c.S3SecretKey, time.Now().UTC())
	resp, err := t.hc.Do(req)
	if err != nil {
		return doErr("S3 "+method+" "+key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError("S3 "+method+" "+key, resp)
	}
	return nil
}

// signS3 adds an AWS Signature V4 Authorization header (host, x-amz-content-sha256 and
// x-amz-date are signed; Content-Type and Cache-Control may be sent unsigned).
func signS3(req *http.Request, body []byte, region, accessKey, secretKey string, now time.Time) {
	if region == "" {
		region = "us-east-1"
	}
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	canonURI := req.URL.EscapedPath()
	if canonURI == "" {
		canonURI = "/"
	}
	host := strings.ToLower(req.URL.Host)
	canonHeaders := "host:" + host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n"
	signed := "host;x-amz-content-sha256;x-amz-date"
	canon := strings.Join([]string{req.Method, canonURI, req.URL.RawQuery, canonHeaders, signed, payload}, "\n")
	ch := sha256.Sum256([]byte(canon))
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// ---- WebDAV (PUT, MKCOL for the page folder inside the configured folder, DELETE) ----

type webdavTarget struct {
	c  Config
	hc *http.Client
}

func (t *webdavTarget) url(p string) string { return t.c.WebDAVURL + "/" + p }

func (t *webdavTarget) req(ctx context.Context, method, p string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.url(p), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if t.c.WebDAVUsername != "" || t.c.WebDAVPassword != "" {
		req.SetBasicAuth(t.c.WebDAVUsername, t.c.WebDAVPassword)
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, doErr("WebDAV "+method+" "+p, err)
	}
	return resp, nil
}

func (t *webdavTarget) put(ctx context.Context, f File) error {
	resp, err := t.req(ctx, http.MethodPut, f.Path, f.Data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict && strings.Contains(f.Path, "/") {
		// The page's folder doesn't exist yet: create it inside the configured folder
		// (the configured folder itself must exist; it is never created).
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck
		dir := path.Dir(f.Path)
		mk, err := t.req(ctx, "MKCOL", dir+"/", nil)
		if err != nil {
			return err
		}
		mk.Body.Close()
		if mk.StatusCode != http.StatusCreated && mk.StatusCode != http.StatusMethodNotAllowed {
			return httpError("WebDAV MKCOL "+dir, mk)
		}
		resp2, err := t.req(ctx, http.MethodPut, f.Path, f.Data)
		if err != nil {
			return err
		}
		defer resp2.Body.Close()
		resp = resp2
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError("WebDAV PUT "+f.Path, resp)
	}
	return nil
}

func (t *webdavTarget) Publish(ctx context.Context, files []File) error {
	for _, f := range files {
		if err := t.put(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

func (t *webdavTarget) Test(ctx context.Context) error {
	if err := t.put(ctx, File{Path: testFile, Data: []byte("Moorage write test\n")}); err != nil {
		return err
	}
	resp, err := t.req(ctx, http.MethodDelete, testFile, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError("WebDAV DELETE "+testFile, resp)
	}
	return nil
}

// ---- GitHub Pages (Git Data API: one commit per publish on an existing branch) ------

type githubTarget struct {
	c   Config
	hc  *http.Client
	api string
}

func (t *githubTarget) call(ctx context.Context, method, p string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.api+"/repos/"+t.c.GitHubRepo+p, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+t.c.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return 0, doErr("GitHub "+method+" "+p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, httpError("GitHub "+method+" "+p, resp)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("GitHub %s %s: %w", method, p, err)
		}
	}
	return resp.StatusCode, nil
}

type ghTreeEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// commit creates a commit with files on top of the branch head and returns its SHA and
// the head it is based on. The branch is not moved.
func (t *githubTarget) commit(ctx context.Context, files []File, message string) (sha, parent string, err error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := t.call(ctx, http.MethodGet, "/git/ref/heads/"+t.c.GitHubBranch, nil, &ref); err != nil {
		return "", "", err
	}
	var head struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := t.call(ctx, http.MethodGet, "/git/commits/"+ref.Object.SHA, nil, &head); err != nil {
		return "", "", err
	}
	entries := make([]ghTreeEntry, 0, len(files))
	for _, f := range files {
		if !utf8.Valid(f.Data) {
			return "", "", fmt.Errorf("%s isn't UTF-8 text", f.Path)
		}
		// Text content inline in the tree: GitHub creates the blobs, one request for all.
		entries = append(entries, ghTreeEntry{Path: t.c.GitHubPath + f.Path, Mode: "100644", Type: "blob", Content: string(f.Data)})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := t.call(ctx, http.MethodPost, "/git/trees", map[string]any{"base_tree": head.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", "", err
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if _, err := t.call(ctx, http.MethodPost, "/git/commits", map[string]any{
		"message": message, "tree": tree.SHA, "parents": []string{ref.Object.SHA},
	}, &c); err != nil {
		return "", "", err
	}
	return c.SHA, ref.Object.SHA, nil
}

func (t *githubTarget) Publish(ctx context.Context, files []File) error {
	for attempt := 0; ; attempt++ {
		sha, _, err := t.commit(ctx, files, "Update status page snapshot")
		if err != nil {
			return err
		}
		// Fast-forward only: someone else pushed meanwhile → build on the new head once more.
		code, err := t.call(ctx, http.MethodPatch, "/git/refs/heads/"+t.c.GitHubBranch, map[string]any{"sha": sha, "force": false}, nil)
		if err == nil {
			return nil
		}
		if code != http.StatusUnprocessableEntity || attempt > 0 {
			return err
		}
	}
}

// Test creates a commit with a test file without moving the branch: creating Git
// objects needs the same write permission as publishing, and nothing becomes visible.
func (t *githubTarget) Test(ctx context.Context) error {
	_, _, err := t.commit(ctx, []File{{Path: testFile, Data: []byte("Moorage write test\n")}}, "Moorage write test (not on any branch)")
	return err
}
