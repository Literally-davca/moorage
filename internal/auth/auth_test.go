package auth

import (
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword("correct horse battery", h); err != nil || !ok {
		t.Fatalf("valid password rejected: %v", err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
}

// RFC 6238 appendix B, SHA-1 vectors (8 digits there; the last 6 digits must match).
func TestTOTPVectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	cases := map[int64]string{
		59:         "287082",
		1111111109: "081804",
		1234567890: "005924",
		2000000000: "279037",
	}
	for ts, want := range cases {
		got, err := totpCode(secret, ts/totpStep)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("t=%d: got %s want %s", ts, got, want)
		}
	}
}

func TestTOTPMatchSkew(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	prev, _ := totpCode(secret, now.Unix()/totpStep-1)
	if _, ok := matchTOTP(secret, prev, now); !ok {
		t.Fatal("previous step should be accepted")
	}
	old, _ := totpCode(secret, now.Unix()/totpStep-3)
	if _, ok := matchTOTP(secret, old, now); ok {
		t.Fatal("code three steps old should be rejected")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, time.Minute)
	if !l.allow("a") || !l.allow("a") {
		t.Fatal("first two attempts must pass")
	}
	if l.allow("a") {
		t.Fatal("third attempt must be blocked")
	}
	if !l.allow("b") {
		t.Fatal("other keys are independent")
	}
}
