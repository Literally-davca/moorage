package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// platformBackend adds RemotePlatformDigests to the fake image backend.
type platformBackend struct {
	*fakeImageBackend
	platforms map[string][]string
	err       error
	calls     atomic.Int32
}

func (p *platformBackend) RemotePlatformDigests(_ context.Context, ref string, _ RegistryCreds) ([]string, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return p.platforms[ref], nil
}

const (
	idxDigest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	amd64Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	arm64Digest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	oldDigest   = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// An image pulled by its platform digest (Moorage's self-update) is up to date against the
// multi-arch :latest, also when the local tag still names the old image; a registry that
// can't answer the index keeps the plain comparison (no false "up to date", no error).
func TestCheckImagesPlatformDigests(t *testing.T) {
	fb := newFakeImageBackend()
	pb := &platformBackend{fakeImageBackend: fb, platforms: map[string][]string{
		"ghcr.io/x/moorage:latest": {amd64Digest, arm64Digest},
		"ghcr.io/x/app:latest":     {amd64Digest, arm64Digest},
	}}
	e := newTestImageEngine(t, fb)
	e.ImageBackend = pb
	writeTestStack(t, e, "moorage", "services:\n  moorage:\n    image: ghcr.io/x/moorage:latest\n")
	writeTestStack(t, e, "app", "services:\n  web:\n    image: ghcr.io/x/app:latest\n  old:\n    image: ghcr.io/x/old:latest\n")

	// Self-update: the tag still names the old image, the container runs the new one,
	// pulled as image@<amd64 digest>.
	fb.local["ghcr.io/x/moorage:latest"] = fakeLocalImage{id: "sha256:oldimg", repoDigests: []string{"ghcr.io/x/moorage@" + oldDigest}, found: true}
	fb.local["sha256:newimg"] = fakeLocalImage{id: "sha256:newimg", repoDigests: []string{"ghcr.io/x/moorage@" + amd64Digest}, found: true}
	fb.remote["ghcr.io/x/moorage:latest"] = fakeRemoteImage{digest: idxDigest}
	fb.containers["moorage"] = []Container{{Service: "moorage", ImageID: "sha256:newimg", State: "running"}}

	// The tag itself was re-tagged to the platform image.
	fb.local["ghcr.io/x/app:latest"] = fakeLocalImage{id: "sha256:appnew", repoDigests: []string{"ghcr.io/x/app@" + arm64Digest}, found: true}
	fb.remote["ghcr.io/x/app:latest"] = fakeRemoteImage{digest: idxDigest}
	// A real update: neither the index nor a platform digest matches.
	fb.local["ghcr.io/x/old:latest"] = fakeLocalImage{id: "sha256:o", repoDigests: []string{"ghcr.io/x/old@" + oldDigest}, found: true}
	fb.remote["ghcr.io/x/old:latest"] = fakeRemoteImage{digest: idxDigest}
	fb.containers["app"] = []Container{{Service: "web", ImageID: "sha256:appnew", State: "running"}, {Service: "old", ImageID: "sha256:o", State: "running"}}

	status := func(stack, svc string) string {
		t.Helper()
		res, err := e.CheckImages(context.Background(), stack)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Service == svc {
				return r.Status
			}
		}
		t.Fatalf("no %s/%s", stack, svc)
		return ""
	}
	if got := status("moorage", "moorage"); got != ImageUpToDate {
		t.Errorf("self-updated moorage: %q, want up to date", got)
	}
	if got := status("app", "web"); got != ImageUpToDate {
		t.Errorf("platform-digest pull: %q, want up to date", got)
	}
	if got := status("app", "old"); got != ImageUpdateAvailable {
		t.Errorf("real update: %q, want update available", got)
	}
	// Cached per index digest: checking again asks the registry no more.
	before := pb.calls.Load()
	_ = status("app", "web")
	if pb.calls.Load() != before {
		t.Errorf("platform digests fetched again for the same index")
	}

	// Registry without the index (or rate-limited): the old, safe comparison.
	fb2 := newFakeImageBackend()
	pb2 := &platformBackend{fakeImageBackend: fb2, err: errors.New("429 too many requests")}
	e2 := newTestImageEngine(t, fb2)
	e2.ImageBackend = pb2
	writeTestStack(t, e2, "app", "services:\n  web:\n    image: ghcr.io/x/app:latest\n")
	fb2.local["ghcr.io/x/app:latest"] = fakeLocalImage{id: "sha256:a", repoDigests: []string{"ghcr.io/x/app@" + arm64Digest}, found: true}
	fb2.remote["ghcr.io/x/app:latest"] = fakeRemoteImage{digest: idxDigest}
	res, err := e2.CheckImages(context.Background(), "app")
	if err != nil || len(res) != 1 || res[0].Status != ImageUpdateAvailable || res[0].Error != "" {
		t.Fatalf("fallback: %+v %v", res, err)
	}
}

func TestRemotePlatformDigestsFromRegistry(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if r.Method != http.MethodGet || r.URL.Path != "/v2/x/app/manifests/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		_, _ = w.Write([]byte(`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
			{"digest":"` + amd64Digest + `","platform":{"os":"linux","architecture":"amd64"}},
			{"digest":"` + arm64Digest + `","platform":{"os":"linux","architecture":"arm64"}},
			{"digest":"not-a-digest"}]}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	host = "localhost:" + host[strings.LastIndex(host, ":")+1:]
	got, err := RemotePlatformDigestsFromRegistry(context.Background(), host+"/x/app:latest", RegistryCreds{}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != amd64Digest || got[1] != arm64Digest {
		t.Fatalf("digests %v", got)
	}
	if !strings.Contains(accept, "image.index") {
		t.Fatalf("Accept %q must ask for the index", accept)
	}
	// A single-platform manifest has no children.
	d, err := parseIndexDigests(strings.NewReader(`{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{}}`))
	if err != nil || len(d) != 0 {
		t.Fatalf("single manifest: %v %v", d, err)
	}
	if _, err := parseIndexDigests(strings.NewReader(`<html>`)); err == nil {
		t.Fatal("garbage accepted")
	}
}
