package backupcrypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func init() { logN = 10 } // fast key derivation in tests

func roundTrip(t *testing.T, size int) {
	t.Helper()
	plain := make([]byte, size)
	rand.Read(plain)
	var enc bytes.Buffer
	w, err := NewWriter(&enc, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(enc.Bytes()), "correct horse")
	if err != nil {
		t.Fatalf("size %d: %v", size, err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("size %d: %v", size, err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("size %d: plaintext differs", size)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17, 2 * chunkSize} {
		roundTrip(t, n)
	}
}

func encrypt(t *testing.T, plain []byte) []byte {
	t.Helper()
	var enc bytes.Buffer
	w, err := NewWriter(&enc, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	w.Close()
	return enc.Bytes()
}

func TestWrongPassword(t *testing.T) {
	enc := encrypt(t, []byte("secret data"))
	if _, err := NewReader(bytes.NewReader(enc), "wrong horse"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("got %v, want ErrWrongPassword", err)
	}
}

func TestNotBackup(t *testing.T) {
	if _, err := NewReader(bytes.NewReader([]byte("\x1f\x8b plain gzip, not a backup at all")), "correct horse"); !errors.Is(err, ErrNotBackup) {
		t.Fatalf("got %v, want ErrNotBackup", err)
	}
}

func TestTruncated(t *testing.T) {
	plain := make([]byte, 3*chunkSize)
	enc := encrypt(t, plain)
	// Cut after the first full chunk: every remaining chunk is gone, so no last flag.
	cut := headerLen + 4 + chunkSize + tagLen
	r, err := NewReader(bytes.NewReader(enc[:cut]), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v, want ErrTruncated", err)
	}
}

func TestTampered(t *testing.T) {
	enc := encrypt(t, make([]byte, 2*chunkSize))
	enc[len(enc)-5] ^= 1
	r, err := NewReader(bytes.NewReader(enc), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("tampered backup read without error")
	}
}

func TestShortPassword(t *testing.T) {
	if _, err := NewWriter(io.Discard, "short"); err == nil {
		t.Fatal("short password accepted")
	}
}
