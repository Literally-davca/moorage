//go:build ignore

// Tool genkey generates an Ed25519 keypair for signing Moorage release manifests.
// Run: go run ./scripts/release/genkey.go
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate Ed25519 keypair: %v\n", err)
		os.Exit(1)
	}
	// priv.Seed() is the compact 32-byte Ed25519 seed; sign.go accepts both 32-byte seed and 64-byte private key.
	seedB64 := base64.StdEncoding.EncodeToString(priv.Seed())
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	fmt.Println("=== Moorage Release Signing Keypair (Ed25519) ===")
	fmt.Println()
	fmt.Println("1) Private key (store ONLY as GitHub Actions secret MOORAGE_RELEASE_KEY):")
	fmt.Printf("   %s\n", seedB64)
	fmt.Println()
	fmt.Println("2) Public key (commit into internal/selfupdate/key.go as releasePublicKeyB64):")
	fmt.Printf("   %s\n", pubB64)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  - Paste the public key into internal/selfupdate/key.go (releasePublicKeyB64)")
	fmt.Println("  - Run: gh secret set MOORAGE_RELEASE_KEY   (paste the private key above)")
	fmt.Println("  - Tag and push your first release: git tag v1.0.0 && git push origin v1.0.0")
}
