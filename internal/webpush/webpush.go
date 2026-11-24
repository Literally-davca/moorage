// Package webpush sends Web Push notifications (RFC 8030) to browsers that subscribed in
// the Moorage UI: payloads are encrypted with aes128gcm (RFC 8291) and requests are
// signed with a VAPID key (RFC 8292). Only the standard library is used.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"moorage/internal/netguard"
	"moorage/internal/notify"
	"moorage/internal/store"
)

var b64 = base64.RawURLEncoding

// ErrGone means the push service dropped the subscription (the browser unsubscribed).
var ErrGone = errors.New("subscription expired")

// Service owns the VAPID key and delivers notifications to every subscription.
type Service struct {
	st      *store.Store
	log     *slog.Logger
	subMu   sync.RWMutex
	subject string // VAPID "sub": the hub's public URL or the project page
	client  *http.Client

	once sync.Once
	key  *ecdsa.PrivateKey
	err  error
}

// SetPublicURL updates the VAPID subject live when the admin sets an HTTPS Public URL.
func (s *Service) SetPublicURL(publicURL string) {
	sub := "https://github.com/Literally-davca/moorage"
	if strings.HasPrefix(publicURL, "https://") {
		sub = strings.TrimRight(publicURL, "/")
	}
	s.subMu.Lock()
	s.subject = sub
	s.subMu.Unlock()
}

func (s *Service) getSubject() string {
	s.subMu.RLock()
	defer s.subMu.RUnlock()
	return s.subject
}
// services; without one the project page is used.
func New(st *store.Store, log *slog.Logger, publicURL string) *Service {
	sub := "https://github.com/Literally-davca/moorage"
	if strings.HasPrefix(publicURL, "https://") {
		sub = strings.TrimRight(publicURL, "/")
	}
	tr := &http.Transport{
		// Endpoints come from browsers, so they are untrusted: public addresses only.
		DialContext:         netguard.DialContext(10*time.Second, false),
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        8,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Service{st: st, log: log, subject: sub, client: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

func (s *Service) loadKey() (*ecdsa.PrivateKey, error) {
	s.once.Do(func() {
		ctx := context.Background()
		if v, err := s.st.GetSetting(ctx, store.SettingVAPIDKey); err == nil && v != "" {
			der, err := base64.StdEncoding.DecodeString(v)
			if err == nil {
				if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
					if ek, ok := k.(*ecdsa.PrivateKey); ok && ek.Curve == elliptic.P256() {
						s.key = ek
						return
					}
				}
			}
			s.log.Warn("web push: stored VAPID key is unreadable, generating a new one (browsers must subscribe again)")
		}
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			s.err = err
			return
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			s.err = err
			return
		}
		if err := s.st.SetSetting(ctx, store.SettingVAPIDKey, base64.StdEncoding.EncodeToString(der)); err != nil {
			s.err = err
			return
		}
		s.key = k
	})
	return s.key, s.err
}

// PublicKey returns the VAPID public key (uncompressed point, base64url) that browsers
// pass to pushManager.subscribe as applicationServerKey.
func (s *Service) PublicKey() (string, error) {
	k, err := s.loadKey()
	if err != nil {
		return "", err
	}
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(pub.Bytes()), nil
}

// ValidateSubscription checks what a browser sent before it is stored.
func ValidateSubscription(endpoint, p256dh, auth string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(endpoint) > 1024 {
		return errors.New("invalid push endpoint")
	}
	pub, err := b64.DecodeString(strings.TrimRight(p256dh, "="))
	if err != nil {
		return errors.New("invalid browser key")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("invalid browser key")
	}
	a, err := b64.DecodeString(strings.TrimRight(auth, "="))
	if err != nil || len(a) != 16 {
		return errors.New("invalid auth secret")
	}
	return nil
}

// payload is what the service worker receives.
type payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Broadcast sends msg to every subscription; dropped subscriptions are removed. event
// names the event kind (notify.Event*); auto-updates are only informational and don't
// go to phones.
func (s *Service) Broadcast(ctx context.Context, event string, msg notify.Message) {
	if s == nil || event == notify.EventAutoUpdate {
		return
	}
	subs, err := s.st.PushSubscriptions(ctx, 0)
	if err != nil || len(subs) == 0 {
		return
	}
	body := msg.Text
	if i := strings.IndexByte(body, '\n'); i > 0 {
		body = body[:i] // first line only: a phone banner is short
	}
	data, _ := json.Marshal(payload{Title: msg.Title, Body: truncate(body, 300), URL: msg.URL, Tag: event})
	for _, sub := range subs {
		err := s.Send(ctx, sub, data)
		if errors.Is(err, ErrGone) {
			_ = s.st.DeletePushSubscription(ctx, 0, sub.Endpoint)
			continue
		}
		if err != nil {
			s.log.Warn("web push failed", "endpoint", endpointHost(sub.Endpoint), "err", err)
		}
	}
}

// SendTest sends a test notification to one user's subscriptions.
func (s *Service) SendTest(ctx context.Context, userID int64) (int, error) {
	subs, err := s.st.PushSubscriptions(ctx, userID)
	if err != nil {
		return 0, err
	}
	data, _ := json.Marshal(payload{Title: "Moorage test notification", Body: "Push notifications reach this device.", Tag: "test"})
	sent := 0
	var last error
	for _, sub := range subs {
		err := s.Send(ctx, sub, data)
		if errors.Is(err, ErrGone) {
			_ = s.st.DeletePushSubscription(ctx, 0, sub.Endpoint)
			continue
		}
		if err != nil {
			last = err
			continue
		}
		sent++
	}
	if sent == 0 && last != nil {
		return 0, last
	}
	return sent, nil
}

// Send encrypts data for one subscription and posts it to the push service.
func (s *Service) Send(ctx context.Context, sub store.PushSubscription, data []byte) error {
	key, err := s.loadKey()
	if err != nil {
		return err
	}
	body, err := encrypt(sub, data)
	if err != nil {
		return err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" {
		return errors.New("invalid push endpoint")
	}
	jwt, err := vapidJWT(key, u.Scheme+"://"+u.Host, s.getSubject(), time.Now())
	if err != nil {
		return err
	}
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+b64.EncodeToString(pub.Bytes()))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) //nolint:errcheck
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 300:
		return fmt.Errorf("push service answered %d", resp.StatusCode)
	}
	return nil
}

// encrypt builds an aes128gcm body (RFC 8291 §3.4, one record).
func encrypt(sub store.PushSubscription, plain []byte) ([]byte, error) {
	uaPubBytes, err := b64.DecodeString(strings.TrimRight(sub.P256DH, "="))
	if err != nil {
		return nil, err
	}
	authSecret, err := b64.DecodeString(strings.TrimRight(sub.Auth, "="))
	if err != nil {
		return nil, err
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubBytes)
	if err != nil {
		return nil, err
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()

	// IKM = HKDF(auth, shared, "WebPush: info\0" || ua_public || as_public, 32)
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	info := "WebPush: info\x00" + string(uaPubBytes) + string(asPub)
	ikm, err := hkdf.Expand(sha256.New, prkKey, info, 32)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := append(append([]byte{}, plain...), 0x02) // last record, no padding
	ct := gcm.Seal(nil, nonce, record, nil)

	const rs = 4096
	if len(ct) > rs-16 {
		return nil, errors.New("push payload too large")
	}
	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(rs)) //nolint:errcheck
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(ct)
	return out.Bytes(), nil
}

// vapidJWT signs the ES256 token push services require (RFC 8292).
func vapidJWT(key *ecdsa.PrivateKey, aud, sub string, now time.Time) (string, error) {
	h := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": now.Add(12 * time.Hour).Unix(), "sub": sub})
	signing := h + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, sg, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sg.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func endpointHost(e string) string {
	if u, err := url.Parse(e); err == nil {
		return u.Host
	}
	return "?"
}
