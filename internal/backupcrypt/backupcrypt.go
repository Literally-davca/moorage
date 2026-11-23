// Package backupcrypt encrypts Moorage backup archives with a password.
//
// Format: a fixed header, then the plaintext in 64 KiB chunks, each sealed with
// AES-256-GCM. The key comes from scrypt(password, salt). Each chunk's nonce is a random
// 7-byte prefix, a 4-byte big-endian counter and a 1-byte "last chunk" flag, so chunks
// can't be reordered, dropped or cut off without the reader noticing. The header is
// authenticated as additional data of every chunk.
//
//	magic "MOORAGE-BACKUP\x00" (15) | version 1 (1) | log2 N (1) | r (1) | p (1) |
//	salt (16) | nonce prefix (7)
//	then per chunk: ciphertext length uint32 BE | ciphertext (plaintext + 16-byte tag)
package backupcrypt

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"
)

const (
	magic      = "MOORAGE-BACKUP\x00"
	version    = 1
	chunkSize  = 64 << 10
	saltLen    = 16
	prefixLen  = 7
	headerLen  = len(magic) + 4 + saltLen + prefixLen
	tagLen     = 16
	maxChunks  = 1<<32 - 1
	minPassLen = 8
)

// scrypt cost. N=2^15, r=8 needs 32 MiB while the key is derived: strong enough for a
// file that may leave the server, small enough for Moorage's memory budget.
var (
	logN uint8 = 15
	cR   uint8 = 8
	cP   uint8 = 1
)

// ErrWrongPassword is returned when the first chunk doesn't authenticate.
var ErrWrongPassword = errors.New("wrong password or damaged backup")

// ErrNotBackup is returned when the data doesn't start with the backup header.
var ErrNotBackup = errors.New("not an encrypted Moorage backup")

// ErrTruncated is returned when the stream ends before its last chunk.
var ErrTruncated = errors.New("backup is incomplete (cut off)")

// CheckPassword rejects passwords too short to protect a backup.
func CheckPassword(p string) error {
	if len([]rune(p)) < minPassLen {
		return fmt.Errorf("the backup password must be at least %d characters", minPassLen)
	}
	return nil
}

func deriveAEAD(password string, salt []byte, n uint8, r, p uint8) (cipher.AEAD, error) {
	if n < 10 || n > 20 || r == 0 || r > 32 || p == 0 || p > 4 {
		return nil, ErrNotBackup
	}
	key, err := scrypt.Key([]byte(password), salt, 1<<n, int(r), int(p), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, counter uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[prefixLen:], counter)
	if last {
		n[11] = 1
	}
	return n
}

type writer struct {
	w       io.Writer
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	buf     []byte
	counter uint32
	closed  bool
	err     error
}

// NewWriter returns a writer that encrypts everything written to it into w. Close must
// be called to write the last chunk; without it the backup can't be read.
func NewWriter(w io.Writer, password string) (io.WriteCloser, error) {
	if err := CheckPassword(password); err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	prefix := make([]byte, prefixLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	aead, err := deriveAEAD(password, salt, logN, cR, cP)
	if err != nil {
		return nil, err
	}
	var h bytes.Buffer
	h.WriteString(magic)
	h.Write([]byte{version, logN, cR, cP})
	h.Write(salt)
	h.Write(prefix)
	if _, err := w.Write(h.Bytes()); err != nil {
		return nil, err
	}
	return &writer{w: w, aead: aead, header: h.Bytes(), prefix: prefix, buf: make([]byte, 0, chunkSize)}, nil
}

func (e *writer) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.closed {
		return 0, errors.New("backupcrypt: write after close")
	}
	n := 0
	for len(p) > 0 {
		room := chunkSize - len(e.buf)
		take := min(room, len(p))
		e.buf = append(e.buf, p[:take]...)
		p = p[take:]
		n += take
		// A full chunk is only sealed once more data follows, so the last chunk can
		// always carry the "last" flag (even when the data is an exact multiple).
		if len(e.buf) == chunkSize && len(p) > 0 {
			if err := e.seal(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *writer) seal(last bool) error {
	if e.counter == maxChunks {
		e.err = errors.New("backupcrypt: backup too large")
		return e.err
	}
	ct := e.aead.Seal(nil, nonce(e.prefix, e.counter, last), e.buf, e.header)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := e.w.Write(l[:]); err != nil {
		e.err = err
		return err
	}
	if _, err := e.w.Write(ct); err != nil {
		e.err = err
		return err
	}
	e.counter++
	e.buf = e.buf[:0]
	return nil
}

func (e *writer) Close() error {
	if e.closed {
		return e.err
	}
	if e.err == nil && len(e.buf) == chunkSize {
		// Seal the full chunk, then an empty last one.
		if err := e.seal(false); err != nil {
			return err
		}
	}
	e.closed = true
	if e.err != nil {
		return e.err
	}
	return e.seal(true)
}

type reader struct {
	r       io.Reader
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	plain   []byte
	counter uint32
	done    bool
}

// NewReader checks the header and the password (by opening the first chunk) and returns
// a reader of the plaintext.
func NewReader(r io.Reader, password string) (io.Reader, error) {
	h := make([]byte, headerLen)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, ErrNotBackup
	}
	if string(h[:len(magic)]) != magic || h[len(magic)] != version {
		return nil, ErrNotBackup
	}
	o := len(magic) + 1
	n, cr, cp := h[o], h[o+1], h[o+2]
	salt := h[o+3 : o+3+saltLen]
	prefix := h[o+3+saltLen:]
	aead, err := deriveAEAD(password, salt, n, cr, cp)
	if err != nil {
		return nil, err
	}
	rd := &reader{r: r, aead: aead, header: h, prefix: prefix}
	if err := rd.next(); err != nil {
		if errors.Is(err, errAuth) {
			return nil, ErrWrongPassword
		}
		return nil, err
	}
	return rd, nil
}

var errAuth = errors.New("auth")

func (d *reader) next() error {
	var l [4]byte
	if _, err := io.ReadFull(d.r, l[:]); err != nil {
		return ErrTruncated
	}
	size := binary.BigEndian.Uint32(l[:])
	if size < tagLen || size > chunkSize+tagLen {
		return ErrTruncated
	}
	ct := make([]byte, size)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return ErrTruncated
	}
	// Try as a middle chunk, then as the last one.
	if pt, err := d.aead.Open(nil, nonce(d.prefix, d.counter, false), ct, d.header); err == nil {
		d.plain = pt
		d.counter++
		return nil
	}
	pt, err := d.aead.Open(nil, nonce(d.prefix, d.counter, true), ct, d.header)
	if err != nil {
		if d.counter == 0 {
			return errAuth
		}
		return errors.New("backup is damaged")
	}
	d.plain = pt
	d.counter++
	d.done = true
	return nil
}

func (d *reader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}
