//go:build ignore

// Tool sign creates a detached Ed25519 signature over an input file (typically SHA256SUMS).
// The private key is read from MOORAGE_RELEASE_KEY (base64 32-byte seed or 64-byte private key).
// Before signing it checks that the key's public half equals releasePublicKeyB64 in
// internal/selfupdate/key.go (override the path with MOORAGE_KEY_FILE): a release signed
// with any other key would be refused by every node, and a release built with an empty
// embedded key could never verify an update.
// Run from the repository root: go run ./scripts/release/sign.go <input-file> <output-sig-file>
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var embeddedKeyRe = regexp.MustCompile(`releasePublicKeyB64\s*=\s*"([^"]*)"`)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <input-file> <output-sig-file>\n", os.Args[0])
		os.Exit(2)
	}
	priv, err := privateKeyFromEnv()
	if err != nil {
		fail(err)
	}
	if err := checkEmbeddedKey(priv.Public().(ed25519.PublicKey)); err != nil {
		fail(err)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail(fmt.Errorf("read %s: %w", os.Args[1], err))
	}
	sig := ed25519.Sign(priv, data)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), data, sig) {
		fail(fmt.Errorf("self-verification failed"))
	}
	out := base64.StdEncoding.EncodeToString(sig) + "\n"
	if err := os.WriteFile(os.Args[2], []byte(out), 0o644); err != nil {
		fail(fmt.Errorf("write %s: %w", os.Args[2], err))
	}
}

func privateKeyFromEnv() (ed25519.PrivateKey, error) {
	rawKey := strings.TrimSpace(os.Getenv("MOORAGE_RELEASE_KEY"))
	if rawKey == "" {
		return nil, fmt.Errorf("MOORAGE_RELEASE_KEY environment variable is empty")
	}
	keyBytes, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, fmt.Errorf("decode MOORAGE_RELEASE_KEY: %w", err)
	}
	switch len(keyBytes) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(keyBytes), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(keyBytes), nil
	default:
		return nil, fmt.Errorf("invalid MOORAGE_RELEASE_KEY length %d (want %d or %d)", len(keyBytes), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// checkEmbeddedKey compares pub with the public key compiled into the release binaries.
func checkEmbeddedKey(pub ed25519.PublicKey) error {
	path := os.Getenv("MOORAGE_KEY_FILE")
	if path == "" {
		path = "internal/selfupdate/key.go"
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read embedded key file %s: %w", path, err)
	}
	m := embeddedKeyRe.FindSubmatch(src)
	if m == nil {
		return fmt.Errorf("releasePublicKeyB64 not found in %s", path)
	}
	embedded := strings.TrimSpace(string(m[1]))
	if embedded == "" {
		return fmt.Errorf("releasePublicKeyB64 in %s is empty: paste the public key from genkey.go before releasing", path)
	}
	derived := base64.StdEncoding.EncodeToString(pub)
	if embedded != derived {
		return fmt.Errorf("MOORAGE_RELEASE_KEY does not match releasePublicKeyB64 in %s (key's public half is %s)", path, derived)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sign:", err)
	os.Exit(1)
}
