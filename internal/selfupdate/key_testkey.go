//go:build moorage_testkey

package selfupdate

import (
	"crypto/ed25519"
	"os"
)

// DefaultPublicKey returns the test Ed25519 public key from MOORAGE_TEST_RELEASE_PUBKEY
// when compiled with -tags moorage_testkey for local live verification.
func DefaultPublicKey() (ed25519.PublicKey, error) {
	if raw := os.Getenv("MOORAGE_TEST_RELEASE_PUBKEY"); raw != "" {
		return decodePublicKey(raw)
	}
	return decodePublicKey(releasePublicKeyB64)
}
