package imagefind

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/distribution/reference"
)

// ImageConfig is the part of an OCI image config the lookup reads.
type ImageConfig struct {
	Config struct {
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Env          []string            `json:"Env"`
		User         string              `json:"User"`
		Healthcheck  *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"config"`
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// registryBase is the v2 API root of the image's registry.
func registryBase(named reference.Named) string {
	d := reference.Domain(named)
	if d == "docker.io" {
		d = "registry-1.docker.io"
	}
	return "https://" + d + "/v2/"
}

// imageConfig reads the image config anonymously: manifest (index → linux/amd64), then
// the config blob. A private image answers 401 and is reported as such.
func (f *Finder) imageConfig(ctx context.Context, named reference.Named) (ImageConfig, error) {
	var cfg ImageConfig
	base := registryBase(named)
	repo := reference.Path(named)
	tag := "latest"
	if t, ok := named.(reference.Tagged); ok {
		tag = t.Tag()
	}
	tok := ""
	fetch := func(path, accept string) ([]byte, string, error) {
		h := http.Header{}
		if accept != "" {
			h.Set("Accept", accept)
		}
		if tok != "" {
			h.Set("Authorization", "Bearer "+tok)
		}
		resp, b, err := f.get(ctx, base+path, h)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && tok == "" {
			t, err := f.token(ctx, resp.Header.Get("WWW-Authenticate"), repo)
			if err != nil {
				return nil, "", err
			}
			tok = t
			if accept != "" {
				h.Set("Accept", accept)
			}
			h.Set("Authorization", "Bearer "+tok)
			resp, b, err = f.get(ctx, base+path, h)
			if err != nil {
				return nil, "", err
			}
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return b, resp.Header.Get("Content-Type"), nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, "", errors.New("the image is private or doesn't exist; paste its compose file instead")
		case http.StatusNotFound:
			return nil, "", errors.New("image or tag not found")
		default:
			return nil, "", errf("registry answered %s", resp.Status)
		}
	}

	b, ctype, err := fetch(repo+"/manifests/"+url.PathEscape(tag), manifestAccept)
	if err != nil {
		return cfg, err
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS, Architecture string
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return cfg, errors.New("unreadable manifest")
	}
	if len(m.Manifests) > 0 || strings.Contains(ctype, "index") || strings.Contains(ctype, "list") {
		pick := ""
		for _, d := range m.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
				pick = d.Digest
				break
			}
		}
		if pick == "" && len(m.Manifests) > 0 {
			pick = m.Manifests[0].Digest
		}
		if pick == "" {
			return cfg, errors.New("the image index lists no images")
		}
		if b, _, err = fetch(repo+"/manifests/"+pick, manifestAccept); err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return cfg, errors.New("unreadable manifest")
		}
	}
	if !strings.HasPrefix(m.Config.Digest, "sha256:") {
		return cfg, errors.New("the manifest has no image config")
	}
	if b, _, err = fetch(repo+"/blobs/"+m.Config.Digest, ""); err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, errors.New("unreadable image config")
	}
	return cfg, nil
}

// token gets an anonymous pull token from the realm named in the challenge. The realm
// must be https.
func (f *Finder) token(ctx context.Context, challenge, repo string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", errors.New("the registry wants a login; paste the compose file instead")
	}
	params := map[string]string{}
	for _, part := range splitChallenge(challenge[len("bearer "):]) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", errors.New("the registry's token service isn't https")
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+repo+":pull")
	realm.RawQuery = q.Encode()
	resp, b, err := f.get(ctx, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("the image is private or doesn't exist; paste its compose file instead")
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &t) != nil {
		return "", errors.New("unreadable registry token")
	}
	if t.Token != "" {
		return t.Token, nil
	}
	return t.AccessToken, nil
}

// splitChallenge splits a WWW-Authenticate parameter list on commas outside quotes.
func splitChallenge(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	return append(out, cur.String())
}
