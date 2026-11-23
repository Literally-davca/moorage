package selfupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"moorage/internal/host"
)

const (
	// MaxBinarySize matches host.SelfUpdateMaxBinarySize (64 MiB).
	MaxBinarySize = host.SelfUpdateMaxBinarySize
	// MaxMetaSize limits release metadata assets (SHA256SUMS, images.json, JSON API).
	MaxMetaSize = 1 << 20
)

// ImagesManifest is the signed container digest & compatibility manifest attached to each release.
type ImagesManifest struct {
	Version     string            `json:"version"`
	Image       string            `json:"image"`
	IndexDigest string            `json:"index_digest,omitempty"`
	Digests     map[string]string `json:"digests"`
	MinHub      string            `json:"min_hub,omitempty"`
	MinAgent    string            `json:"min_agent,omitempty"`
}

// VerifySums verifies sig (raw 64 bytes or base64-encoded signature) over exact sums bytes
// using pub, and parses SHA256SUMS entries ("<hex>  <filename>") into a map.
func VerifySums(sums, sig []byte, pub ed25519.PublicKey) (map[string]string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrReleaseKeyUnconfigured
	}
	if len(sums) == 0 || len(sums) > MaxMetaSize {
		return nil, fmt.Errorf("%w: invalid SHA256SUMS size %d", host.ErrSelfUpdateInvalid, len(sums))
	}
	sigBytes, err := decodeSig(sig)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
	}
	if !ed25519.Verify(pub, sums, sigBytes) {
		return nil, fmt.Errorf("%w: Ed25519 signature verification failed", host.ErrSelfUpdateInvalid)
	}
	parsed, err := ParseSums(sums)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", host.ErrSelfUpdateInvalid, err)
	}
	return parsed, nil
}

func decodeSig(sig []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(sig))
	if len(sig) == ed25519.SignatureSize {
		return sig, nil
	}
	if trimmed == "" {
		return nil, errors.New("empty signature")
	}
	b, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	if len(b) != ed25519.SignatureSize {
		return nil, fmt.Errorf("bad signature length %d", len(b))
	}
	return b, nil
}

// ParseSums parses sha256sum output lines ("<64-hex>  <name>" or "<64-hex> *<name>").
func ParseSums(sums []byte) (map[string]string, error) {
	out := make(map[string]string)
	for rawLine := range strings.SplitSeq(strings.ReplaceAll(string(sums), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed SHA256SUMS line %q", line)
		}
		digestHex := strings.ToLower(fields[0])
		if len(digestHex) != 64 {
			return nil, fmt.Errorf("invalid SHA-256 hex length in %q", line)
		}
		if _, err := hex.DecodeString(digestHex); err != nil {
			return nil, fmt.Errorf("invalid SHA-256 hex in %q: %w", line, err)
		}
		name := strings.TrimPrefix(fields[1], "*")
		name = strings.TrimPrefix(name, "./")
		name = filepath.Base(name)
		if name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("invalid filename in SHA256SUMS: %q", fields[1])
		}
		out[name] = digestHex
	}
	if len(out) == 0 {
		return nil, errors.New("empty SHA256SUMS")
	}
	return out, nil
}

// VerifyManifest verifies the Ed25519 signature over SHA256SUMS and confirms that images.json
// matches the checksum inside SHA256SUMS and names m.Version.
func VerifyManifest(m host.SelfUpdateManifest, pub ed25519.PublicKey) (map[string]string, ImagesManifest, error) {
	sums, err := VerifySums(m.Sums, m.Sig, pub)
	if err != nil {
		return nil, ImagesManifest{}, err
	}
	wantImagesHex, ok := sums["images.json"]
	if !ok {
		return nil, ImagesManifest{}, fmt.Errorf("%w: images.json missing from signed SHA256SUMS", host.ErrSelfUpdateInvalid)
	}
	if len(m.ImagesJSON) == 0 || len(m.ImagesJSON) > MaxMetaSize {
		return nil, ImagesManifest{}, fmt.Errorf("%w: invalid images.json size", host.ErrSelfUpdateInvalid)
	}
	gotSum := sha256.Sum256(m.ImagesJSON)
	if hex.EncodeToString(gotSum[:]) != wantImagesHex {
		return nil, ImagesManifest{}, fmt.Errorf("%w: images.json checksum mismatch", host.ErrSelfUpdateInvalid)
	}
	var img ImagesManifest
	if err := json.Unmarshal(m.ImagesJSON, &img); err != nil {
		return nil, ImagesManifest{}, fmt.Errorf("%w: parse images.json: %v", host.ErrSelfUpdateInvalid, err)
	}
	if strings.TrimPrefix(strings.TrimSpace(img.Version), "v") != strings.TrimPrefix(strings.TrimSpace(m.Version), "v") {
		return nil, ImagesManifest{}, fmt.Errorf("%w: manifest version %q does not match signed images.json version %q",
			host.ErrSelfUpdateInvalid, m.Version, img.Version)
	}
	return sums, img, nil
}

// VerifyContainerDigest verifies the signed manifest and ensures m.Image and m.Digest match
// the signed images.json digest for platform (e.g. linux/amd64).
func VerifyContainerDigest(m host.SelfUpdateManifest, platform string, pub ed25519.PublicKey) (ImagesManifest, string, error) {
	_, img, err := VerifyManifest(m, pub)
	if err != nil {
		return ImagesManifest{}, "", err
	}
	if img.Image == "" {
		return ImagesManifest{}, "", fmt.Errorf("%w: signed images.json has no image repository", host.ErrSelfUpdateInvalid)
	}
	if m.Image != "" && m.Image != img.Image {
		return ImagesManifest{}, "", fmt.Errorf("%w: image repository %q does not match signed %q", host.ErrSelfUpdateInvalid, m.Image, img.Image)
	}
	wantDigest := strings.TrimSpace(img.Digests[platform])
	if wantDigest == "" {
		return ImagesManifest{}, "", fmt.Errorf("%w: no signed digest for platform %s", host.ErrSelfUpdateInvalid, platform)
	}
	if m.Digest != "" && m.Digest != wantDigest {
		return ImagesManifest{}, "", fmt.Errorf("%w: digest %q does not match signed digest %q for %s",
			host.ErrSelfUpdateInvalid, m.Digest, wantDigest, platform)
	}
	return img, wantDigest, nil
}

// VerifyReaderToTemp streams r into a new temporary file inside dir, enforcing maxBytes and
// verifying its SHA-256 matches expectedHex before marking it 0755 and syncing to disk.
func VerifyReaderToTemp(dir string, r io.Reader, expectedHex string, maxBytes int64) (string, error) {
	expectedHex = strings.ToLower(strings.TrimSpace(expectedHex))
	if len(expectedHex) != 64 {
		return "", fmt.Errorf("%w: invalid expected sha256", host.ErrSelfUpdateInvalid)
	}
	f, err := os.CreateTemp(dir, ".moorage-update-*")
	if err != nil {
		return "", fmt.Errorf("create staging file: %w", err)
	}
	tmpPath := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	limited := io.LimitReader(r, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		return "", fmt.Errorf("stream binary: %w", err)
	}
	if n > maxBytes {
		return "", fmt.Errorf("%w: binary exceeds %d bytes", host.ErrSelfUpdateInvalid, maxBytes)
	}
	if n == 0 {
		return "", fmt.Errorf("%w: empty binary payload", host.ErrSelfUpdateInvalid)
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if gotHex != expectedHex {
		return "", fmt.Errorf("%w: SHA-256 mismatch (got %s, want %s)", host.ErrSelfUpdateInvalid, gotHex, expectedHex)
	}
	if err := f.Chmod(0o755); err != nil {
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync staged binary: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close staged binary: %w", err)
	}
	cleanup = false
	return tmpPath, nil
}
