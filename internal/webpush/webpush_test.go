package webpush

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"moorage/internal/store"
)

// decrypt plays the browser: it reverses encrypt with the subscription's private key.
func decrypt(t *testing.T, uaPriv *ecdh.PrivateKey, authSecret, body []byte) []byte {
	t.Helper()
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	if rs != 4096 {
		t.Fatalf("rs %d", rs)
	}
	idLen := int(body[20])
	asPubBytes := body[21 : 21+idLen]
	ct := body[21+idLen:]
	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := uaPriv.ECDH(asPub)
	if err != nil {
		t.Fatal(err)
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, authSecret)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPriv.PublicKey().Bytes())+string(asPubBytes), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if pt[len(pt)-1] != 0x02 {
		t.Fatal("missing last-record delimiter")
	}
	return pt[:len(pt)-1]
}

func TestEncryptRoundTrip(t *testing.T) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	authSecret := make([]byte, 16)
	rand.Read(authSecret)
	sub := store.PushSubscription{
		Endpoint: "https://push.example.com/abc",
		P256DH:   b64.EncodeToString(uaPriv.PublicKey().Bytes()),
		Auth:     b64.EncodeToString(authSecret),
	}
	if err := ValidateSubscription(sub.Endpoint, sub.P256DH, sub.Auth); err != nil {
		t.Fatal(err)
	}
	msg := []byte(`{"title":"Check down: web","body":"HTTP 502"}`)
	body, err := encrypt(sub, msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := decrypt(t, uaPriv, authSecret, body); string(got) != string(msg) {
		t.Fatalf("got %q", got)
	}
}

func TestVAPIDJWT(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok, err := vapidJWT(key, "https://push.example.com", "https://moorage.example.com", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	var claims map[string]any
	raw, _ := b64.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &claims); err != nil || claims["aud"] != "https://push.example.com" {
		t.Fatalf("claims %s", raw)
	}
	sig, _ := b64.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.PublicKey, sum[:], r, s) {
		t.Fatal("signature doesn't verify")
	}
}

func TestValidateSubscription(t *testing.T) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	good := b64.EncodeToString(uaPriv.PublicKey().Bytes())
	auth := b64.EncodeToString(make([]byte, 16))
	for _, c := range []struct{ ep, key, auth string }{
		{"http://push.example.com/x", good, auth},
		{"https://user:pw@push.example.com/x", good, auth},
		{"https://push.example.com/x", "bad", auth},
		{"https://push.example.com/x", good, b64.EncodeToString(make([]byte, 8))},
	} {
		if ValidateSubscription(c.ep, c.key, c.auth) == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
