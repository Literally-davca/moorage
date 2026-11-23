package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// allowedAlgs are the ID token signatures Moorage accepts. "none" and HMAC are never
// accepted: an HMAC "signed" with the provider's public key would be forgeable.
var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.PS256}

// AllowedAlgNames is allowedAlgs for the go-oidc verifier.
var allowedAlgNames = []string{string(jose.RS256), string(jose.ES256), string(jose.PS256)}

// keySet is the provider's JWKS. go-oidc's RemoteKeySet refetches on every failed
// verification, also a bad signature; this one refetches only for an unknown key id and
// at most once per minGap, so a stream of bad tokens can't hammer the provider.
type keySet struct {
	url    string
	client *http.Client
	minGap time.Duration

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time
	fetches   int // for tests
}

// VerifySignature implements oidc.KeySet.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, allowedAlgs)
	if err != nil {
		return nil, fmt.Errorf("id token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("id token: want exactly one signature")
	}
	kid := jws.Signatures[0].Header.KeyID
	keys, err := k.keysFor(ctx, kid)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		if kid != "" && keys[i].KeyID != kid {
			continue
		}
		if payload, err := jws.Verify(&keys[i]); err == nil {
			return payload, nil
		}
	}
	return nil, errors.New("id token: signature is not valid")
}

// keysFor returns the cached keys, refetching first when kid is unknown (key rotation).
func (k *keySet) keysFor(ctx context.Context, kid string) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fetchedAt.IsZero() || !hasKey(k.keys, kid) {
		if !k.fetchedAt.IsZero() && time.Since(k.fetchedAt) < k.minGap {
			return k.keys, nil // refetched moments ago; the key really is unknown
		}
		keys, err := k.fetch(ctx)
		k.fetchedAt = time.Now()
		k.fetches++
		if err != nil {
			if k.keys != nil {
				return k.keys, nil
			}
			return nil, err
		}
		k.keys = keys
	}
	return k.keys, nil
}

func hasKey(keys []jose.JSONWebKey, kid string) bool {
	if kid == "" {
		return len(keys) > 0
	}
	for _, key := range keys {
		if key.KeyID == kid {
			return true
		}
	}
	return false
}

func (k *keySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("read jwks: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, r := range raw.Keys {
		var key jose.JSONWebKey
		// Keys of unknown types or for encryption are skipped, not fatal: providers list
		// more than signing keys.
		if err := json.Unmarshal(r, &key); err != nil || !key.Valid() || !key.IsPublic() || (key.Use != "" && key.Use != "sig") {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks has no usable signing key")
	}
	return keys, nil
}
