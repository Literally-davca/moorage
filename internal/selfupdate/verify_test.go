package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"moorage/internal/host"
)

type signedFixture struct {
	pub        ed25519.PublicKey
	priv       ed25519.PrivateKey
	version    string
	binData    []byte
	binHex     string
	assetName  string
	platform   string
	imageRepo  string
	digest     string
	imagesJSON []byte
	sums       []byte
	sigB64     []byte
	manifest   host.SelfUpdateManifest
}

func makeSignedFixture(t *testing.T, ver, assetName, platform string, binData []byte, minHub, minAgent string) signedFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(binData) == 0 {
		binData = []byte("#!/bin/sh\necho moorage-" + ver + "\n")
	}
	binSum := sha256.Sum256(binData)
	binHex := hex.EncodeToString(binSum[:])
	imgDigest := "sha256:" + hex.EncodeToString(sha256sum([]byte("image-"+ver+"-"+platform)))

	imgDoc := ImagesManifest{
		Version:     ver,
		Image:       "ghcr.io/literally-davca/moorage",
		IndexDigest: "sha256:" + hex.EncodeToString(sha256sum([]byte("index-"+ver))),
		Digests: map[string]string{
			platform: imgDigest,
		},
		MinHub:   minHub,
		MinAgent: minAgent,
	}
	imagesJSON, err := json.MarshalIndent(imgDoc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	imgHex := hex.EncodeToString(sha256sum(imagesJSON))
	sums := fmt.Appendf(nil, "%s  images.json\n%s  %s\n", imgHex, binHex, assetName)
	rawSig := ed25519.Sign(priv, sums)
	sigB64 := []byte(base64.StdEncoding.EncodeToString(rawSig) + "\n")

	m := host.SelfUpdateManifest{
		Version:    ver,
		Sums:       sums,
		Sig:        sigB64,
		ImagesJSON: imagesJSON,
		Image:      imgDoc.Image,
		Digest:     imgDigest,
	}
	return signedFixture{
		pub:        pub,
		priv:       priv,
		version:    ver,
		binData:    binData,
		binHex:     binHex,
		assetName:  assetName,
		platform:   platform,
		imageRepo:  imgDoc.Image,
		digest:     imgDigest,
		imagesJSON: imagesJSON,
		sums:       sums,
		sigB64:     sigB64,
		manifest:   m,
	}
}

func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestVerifySignaturesAndChecksums(t *testing.T) {
	fx := makeSignedFixture(t, "1.2.0", "moorage-linux-amd64", "linux/amd64", []byte("binary-payload-v1.2.0"), "", "")

	// Good signature + base64 sig
	sums, img, err := VerifyManifest(fx.manifest, fx.pub)
	if err != nil {
		t.Fatalf("expected valid manifest, got %v", err)
	}
	if sums[fx.assetName] != fx.binHex {
		t.Fatalf("unexpected binary digest %q != %q", sums[fx.assetName], fx.binHex)
	}
	if img.Digests["linux/amd64"] != fx.digest {
		t.Fatalf("unexpected image digest")
	}

	// Raw 64-byte signature also accepted
	rawManifest := fx.manifest
	rawSigBytes, _ := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(fx.sigB64)))
	rawManifest.Sig = rawSigBytes
	if _, _, err := VerifyManifest(rawManifest, fx.pub); err != nil {
		t.Fatalf("raw 64-byte sig failed: %v", err)
	}

	// Bad signature
	badSig := fx.manifest
	badSig.Sig = []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, ed25519.SignatureSize)))
	if _, _, err := VerifyManifest(badSig, fx.pub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for bad signature, got %v", err)
	}

	// Wrong public key
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := VerifyManifest(fx.manifest, otherPub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for wrong key, got %v", err)
	}

	// Unconfigured key
	if _, _, err := VerifyManifest(fx.manifest, nil); !errors.Is(err, ErrReleaseKeyUnconfigured) {
		t.Fatalf("expected ErrReleaseKeyUnconfigured, got %v", err)
	}

	// Tampered SHA256SUMS
	tamperedSums := fx.manifest
	tamperedSums.Sums = bytes.ReplaceAll(append([]byte(nil), fx.sums...), []byte(fx.assetName), []byte("moorage-evil"))
	if _, _, err := VerifyManifest(tamperedSums, fx.pub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for tampered SHA256SUMS, got %v", err)
	}

	// Tampered images.json
	tamperedImg := fx.manifest
	tamperedImg.ImagesJSON = bytes.ReplaceAll(append([]byte(nil), fx.imagesJSON...), []byte(fx.digest), []byte("sha256:0000"))
	if _, _, err := VerifyManifest(tamperedImg, fx.pub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for tampered images.json, got %v", err)
	}

	// Tampered image digest not in signed images.json
	badDigest := fx.manifest
	badDigest.Digest = "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if _, _, err := VerifyContainerDigest(badDigest, "linux/amd64", fx.pub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for unsigned image digest, got %v", err)
	}

	// Wrong image repo
	badRepo := fx.manifest
	badRepo.Image = "evil.invalid/attacker/moorage"
	if _, _, err := VerifyContainerDigest(badRepo, "linux/amd64", fx.pub); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected invalid error for wrong repo, got %v", err)
	}

	// Valid container digest
	if _, gotDigest, err := VerifyContainerDigest(fx.manifest, "linux/amd64", fx.pub); err != nil || gotDigest != fx.digest {
		t.Fatalf("expected good container verify, got digest=%q err=%v", gotDigest, err)
	}

	// Binary stream verification: good vs tampered vs oversize
	dir := t.TempDir()
	tmpGood, err := VerifyReaderToTemp(dir, bytes.NewReader(fx.binData), fx.binHex, 1<<20)
	if err != nil {
		t.Fatalf("good stream failed: %v", err)
	}
	if st, serr := os.Stat(tmpGood); serr != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("expected 0755 staged file, got stat=%v mode=%v", serr, st.Mode().Perm())
	}

	// Tampered binary must fail and leave no leftover file in dir
	entriesBefore, _ := os.ReadDir(dir)
	if _, err := VerifyReaderToTemp(dir, bytes.NewReader(append(fx.binData, 'X')), fx.binHex, 1<<20); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected checksum mismatch for tampered binary, got %v", err)
	}
	entriesAfter, _ := os.ReadDir(dir)
	if len(entriesBefore) != len(entriesAfter) {
		t.Fatalf("tampered stream left temp file behind")
	}

	// Size limit enforcement
	if _, err := VerifyReaderToTemp(dir, bytes.NewReader(bytes.Repeat([]byte("A"), 200)), fx.binHex, 100); !errors.Is(err, host.ErrSelfUpdateInvalid) {
		t.Fatalf("expected size cap error, got %v", err)
	}
}
