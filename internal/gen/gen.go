// Package gen generates random values for @generate(...) keywords, Coolify magic
// variables and template variables (M18E). It is a leaf package (crypto/rand only).
package gen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Generated value kinds of @generate(...) (M18E). One generator serves templates
// (generate: in template.yaml), the MCP generateSecrets argument and the keywords in
// compose.yaml / .env.
const (
	KindPassword  = "password"  // password[, length][, symbols]: default 32 of [A-Za-z0-9]
	KindAlnum     = "alnum"     // alnum[, length]
	KindHex       = "hex"       // hex[, bytes]: default 16 bytes = 32 characters
	KindBase64    = "base64"    // base64[, bytes]: default 32 bytes, standard encoding
	KindBase64Key = "base64key" // "base64:" + 32 random bytes (Laravel APP_KEY)
	KindUUID      = "uuid"      // random UUID v4
	KindUser      = "user"      // u_ + 8 lowercase alphanumerics
	KindLowerUser = "lowercaseuser"
	KindPort      = "port"  // port[, preferred]: chosen by the caller (free host port)
	KindHex32     = "hex32" // template.yaml legacy: 32 hex characters
)

// Length limits of the generated values (characters or bytes).
const (
	MinGenLen = 8
	MaxGenLen = 256
)

const (
	alnum      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
	// symbols safe in .env and URLs without quoting: no quotes, $, #, \ or spaces.
	withSymbols = alnum + "!%*+,-./:;<=>?@[]^_{|}~"
)

// GenSpec is one parsed @generate(kind[, params...]).
type GenSpec struct {
	Kind   string
	Params []string
}

func (g GenSpec) String() string {
	if len(g.Params) == 0 {
		return g.Kind
	}
	return g.Kind + ", " + strings.Join(g.Params, ", ")
}

// ParseGenerate reads "@generate(kind, p1, p2)" (the whole string, spaces allowed). ok is
// false when s is not a keyword at all; err reports a keyword that is malformed or names
// an unknown kind.
func ParseGenerate(s string) (spec GenSpec, ok bool, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "@generate(") {
		return spec, false, nil
	}
	if !strings.HasSuffix(s, ")") {
		return spec, true, fmt.Errorf("%q: missing closing parenthesis", s)
	}
	inner := s[len("@generate(") : len(s)-1]
	parts := strings.Split(inner, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	spec = GenSpec{Kind: strings.ToLower(parts[0]), Params: parts[1:]}
	return spec, true, spec.Validate()
}

// ContainsGenerate reports whether s contains a @generate keyword anywhere.
func ContainsGenerate(s string) bool { return strings.Contains(s, "@generate(") }

// Validate checks the kind and its parameters.
func (g GenSpec) Validate() error {
	num := func(i, def, lo, hi int) (int, error) {
		if i >= len(g.Params) || g.Params[i] == "" {
			return def, nil
		}
		n, err := strconv.Atoi(g.Params[i])
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("@generate(%s): %q must be a number from %d to %d", g, g.Params[i], lo, hi)
		}
		return n, nil
	}
	maxParams := map[string]int{KindPassword: 2, KindAlnum: 1, KindHex: 1, KindBase64: 1, KindBase64Key: 0, KindUUID: 0, KindUser: 0, KindLowerUser: 0, KindPort: 1, KindHex32: 0}
	mp, known := maxParams[g.Kind]
	if !known {
		return fmt.Errorf("@generate: unknown kind %q (use password, alnum, hex, base64, base64key, uuid, user or port)", g.Kind)
	}
	if len(g.Params) > mp {
		return fmt.Errorf("@generate(%s): too many parameters", g)
	}
	switch g.Kind {
	case KindPassword, KindAlnum, KindHex, KindBase64:
		if _, err := num(0, 0, MinGenLen, MaxGenLen); err != nil {
			return err
		}
		if g.Kind == KindPassword && len(g.Params) == 2 && g.Params[1] != "symbols" {
			return fmt.Errorf("@generate(%s): the second parameter can only be \"symbols\"", g)
		}
	case KindPort:
		if _, err := num(0, 0, 1, 65535); err != nil {
			return err
		}
	}
	return nil
}

// PreferredPort is the port a port spec asks for (0 = any).
func (g GenSpec) PreferredPort() int {
	if len(g.Params) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(g.Params[0])
	return n
}

// Value generates the value with crypto/rand. Ports are chosen by the caller.
func (g GenSpec) Value() (string, error) {
	if err := g.Validate(); err != nil {
		return "", err
	}
	n := func(def int) int {
		if len(g.Params) > 0 && g.Params[0] != "" {
			v, _ := strconv.Atoi(g.Params[0])
			return v
		}
		return def
	}
	switch g.Kind {
	case KindPassword:
		if len(g.Params) == 2 {
			return randomString(withSymbols, n(32))
		}
		return randomString(alnum, n(32))
	case KindAlnum:
		return randomString(alnum, n(32))
	case KindHex:
		b, err := randomBytes(n(16))
		return hex.EncodeToString(b), err
	case KindHex32:
		b, err := randomBytes(16)
		return hex.EncodeToString(b), err
	case KindBase64:
		b, err := randomBytes(n(32))
		return base64.StdEncoding.EncodeToString(b), err
	case KindBase64Key:
		b, err := randomBytes(32)
		return "base64:" + base64.StdEncoding.EncodeToString(b), err
	case KindUUID:
		b, err := randomBytes(16)
		if err != nil {
			return "", err
		}
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		h := hex.EncodeToString(b)
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
	case KindUser:
		s, err := randomString(lowerAlnum, 8)
		return "u_" + s, err
	case KindLowerUser:
		return randomString(lowerAlnum, 16)
	case KindPort:
		return "", fmt.Errorf("@generate(port) is chosen from the host's free ports")
	}
	return "", fmt.Errorf("@generate: unknown kind %q", g.Kind)
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random: %w", err)
	}
	return b, nil
}

// CoolifySpec maps a Coolify "magic" variable name (SERVICE_PASSWORD_DB, …) to the value it
// stands for, following Coolify's documented list. ok is false for other names.
// SERVICE_URL_* / SERVICE_FQDN_* / SERVICE_PORT_* return their own kinds ("url",
// "fqdn", "port"): the caller fills them from the host address and free ports.
func CoolifySpec(name string) (spec GenSpec, ok bool) {
	if !strings.HasPrefix(name, "SERVICE_") {
		return spec, false
	}
	rest := name[len("SERVICE_"):]
	type pat struct {
		prefix string
		spec   GenSpec
	}
	// longest prefixes first
	pats := []pat{
		{"PASSWORDWITHSYMBOLS_64_", GenSpec{KindPassword, []string{"64", "symbols"}}},
		{"PASSWORDWITHSYMBOLS_", GenSpec{KindPassword, []string{"32", "symbols"}}},
		{"PASSWORD_64_", GenSpec{KindPassword, []string{"64"}}},
		{"PASSWORD_", GenSpec{KindPassword, []string{"32"}}},
		{"LOWERCASEUSER_", GenSpec{KindLowerUser, nil}},
		{"USER_", GenSpec{KindAlnum, []string{"16"}}},
		{"REALBASE64_128_", GenSpec{KindBase64, []string{"128"}}},
		{"REALBASE64_64_", GenSpec{KindBase64, []string{"64"}}},
		{"REALBASE64_32_", GenSpec{KindBase64, []string{"32"}}},
		{"REALBASE64_", GenSpec{KindBase64, []string{"32"}}},
		{"BASE64_128_", GenSpec{KindAlnum, []string{"128"}}},
		{"BASE64_64_", GenSpec{KindAlnum, []string{"64"}}},
		{"BASE64_32_", GenSpec{KindAlnum, []string{"32"}}},
		{"BASE64_", GenSpec{KindAlnum, []string{"32"}}},
		{"HEX_128_", GenSpec{KindHex, []string{"64"}}},
		{"HEX_64_", GenSpec{KindHex, []string{"32"}}},
		{"HEX_32_", GenSpec{KindHex, []string{"16"}}},
		{"PORT_", GenSpec{KindPort, nil}},
		{"URL_", GenSpec{"url", nil}},
		{"FQDN_", GenSpec{"fqdn", nil}},
	}
	for _, p := range pats {
		if strings.HasPrefix(rest, p.prefix) && len(rest) > len(p.prefix) {
			return p.spec, true
		}
	}
	return spec, false
}

// randomString draws n characters uniformly from alphabet (rejection sampling, no modulo bias).
func randomString(alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, n*2)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		for _, x := range buf {
			if int(x) < limit && len(out) < n {
				out = append(out, alphabet[int(x)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

// Kind generates a value from a bare kind with optional comma parameters
// ("password", "hex, 32"), as template.yaml and the MCP generateSecrets argument use.
func Kind(kind string) (string, error) {
	spec, _, err := ParseGenerate("@generate(" + kind + ")")
	if err != nil {
		return "", err
	}
	return spec.Value()
}
