//go:build !moorage_testkey

package selfupdate

import "crypto/ed25519"

// DefaultPublicKey returns the compiled-in Ed25519 public key used to verify release signatures.
// Release builds never allow environment overrides of this key.
func DefaultPublicKey() (ed25519.PublicKey, error) {
	return decodePublicKey(releasePublicKeyB64)
}
