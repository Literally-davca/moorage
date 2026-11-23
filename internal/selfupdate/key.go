// Package selfupdate verifies, downloads, applies, and rolls back signed Moorage updates
// across the hub/all-in-one server and connected agents.
package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// releasePublicKeyB64 is the base64-encoded Ed25519 public key embedded in release binaries.
// Populate it from scripts/release/genkey.go before tagging the first signed release;
// the matching private key lives ONLY in the GitHub Actions secret MOORAGE_RELEASE_KEY.
const releasePublicKeyB64 = ""

// ErrReleaseKeyUnconfigured indicates that releasePublicKeyB64 has not been populated yet.
var ErrReleaseKeyUnconfigured = errors.New("release signing public key is not configured in this build")

func decodePublicKey(raw string) (ed25519.PublicKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrReleaseKeyUnconfigured
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode release public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid release public key length %d (want %d)", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
