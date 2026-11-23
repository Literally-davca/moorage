// Package tunnel connects agents to the hub: a private certificate authority,
// one-time join tokens, and a mutual-TLS WebSocket carrying multiplexed streams.
//
// The agent always dials out to the hub (works behind NAT); the hub then sends
// HTTP requests to the agent through streams of that one connection.
package tunnel

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HubServerName is the name in the hub's TLS certificate. Agents verify it
// instead of the address they dial, so any IP or DNS name works.
const HubServerName = "moorage-hub"

// CA is the hub's private certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// LoadOrCreateCA reads ca.crt/ca.key from dir or creates them.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if _, err := os.Stat(certPath); err == nil {
		cert, err := readCert(certPath)
		if err != nil {
			return nil, err
		}
		key, err := readKey(keyPath)
		if err != nil {
			return nil, err
		}
		return &CA{Cert: cert, Key: key}, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Moorage hub CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(30, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	if err := writeKey(keyPath, key); err != nil {
		return nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// Fingerprint identifies the CA inside join tokens (SHA-256 of the certificate).
func (ca *CA) Fingerprint() string { return fingerprint(ca.Cert.Raw) }

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ServerCertificate issues a fresh TLS certificate for the hub's agent port.
func (ca *CA) ServerCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: HubServerName},
		DNSNames:     []string{HubServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key}, nil
}

// SignAgent turns an agent's certificate request into a client certificate
// whose common name is the agent id.
func (ca *CA) SignAgent(csrDER []byte, agentID string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("certificate request: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: agentID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0), // revocation = removing the agent on the hub
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
}

// SignPeer turns an agent's peer CSR into a 1-year certificate valid for both
// server and client auth between watchdog peers (CN and DNSName = agentID).
func (ca *CA) SignPeer(csrDER []byte, agentID string) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("certificate request: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: agentID},
		DNSNames:     []string{agentID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
}

// PeerCertValid reports whether der is a valid peer certificate for agentID
// that still has at least 30 days left before expiry.
func PeerCertValid(der []byte, agentID string, now time.Time) bool {
	if len(der) == 0 {
		return false
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil || cert.Subject.CommonName != agentID {
		return false
	}
	return now.Add(30 * 24 * time.Hour).Before(cert.NotAfter)
}

// ---- join tokens -----------------------------------------------------------

const tokenPrefix = "mj1"

// NewJoinToken returns a token (secret + CA fingerprint) and the hash to store.
func NewJoinToken(ca *CA) (token, hash string, err error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(b)
	return tokenPrefix + "." + secret + "." + ca.Fingerprint(), HashSecret(secret), nil
}

// HashSecret hashes the secret part of a join token for storage.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ParseJoinToken splits a token into its secret and the expected CA fingerprint.
func ParseJoinToken(tok string) (secret, caFP string, err error) {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix || len(parts[1]) != 32 || len(parts[2]) != 43 {
		return "", "", errors.New("the join token is malformed, copy it again from the hub")
	}
	return parts[1], parts[2], nil
}

// ---- files -----------------------------------------------------------------

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	var buf bytes.Buffer
	if err := pem.Encode(&buf, &pem.Block{Type: typ, Bytes: der}); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "PRIVATE KEY", der, 0o600)
}

func readPEM(path, typ string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("%s: no %s", path, typ)
	}
	return blk.Bytes, nil
}

func readCert(path string) (*x509.Certificate, error) {
	der, err := readPEM(path, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	der, err := readPEM(path, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA key", path)
	}
	return ek, nil
}
