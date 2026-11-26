package gen

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestParseAndValue(t *testing.T) {
	cases := []struct {
		in   string
		re   string
		size int
	}{
		{"@generate(password)", `^[A-Za-z0-9]{32}$`, 0},
		{"@generate(password, 48)", `^[A-Za-z0-9]{48}$`, 0},
		{"@generate( alnum , 12 )", `^[A-Za-z0-9]{12}$`, 0},
		{"@generate(hex)", `^[0-9a-f]{32}$`, 0},
		{"@generate(hex, 32)", `^[0-9a-f]{64}$`, 0},
		{"@generate(uuid)", `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, 0},
		{"@generate(user)", `^u_[a-z0-9]{8}$`, 0},
		{"@generate(base64key)", `^base64:`, 32},
		{"@generate(base64, 16)", ``, 16},
	}
	for _, c := range cases {
		spec, ok, err := ParseGenerate(c.in)
		if !ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", c.in, ok, err)
		}
		v, err := spec.Value()
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if c.re != "" && !regexp.MustCompile(c.re).MatchString(v) {
			t.Errorf("%s: %q does not match %s", c.in, v, c.re)
		}
		if c.size > 0 {
			b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "base64:"))
			if err != nil || len(b) != c.size {
				t.Errorf("%s: %q decodes to %d bytes (%v)", c.in, v, len(b), err)
			}
		}
	}
	v, _ := GenSpec{KindPassword, []string{"64", "symbols"}}.Value()
	if len(v) != 64 || strings.ContainsAny(v, "\"'$#\\ ") {
		t.Errorf("symbols password %q", v)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"@generate(nope)", "@generate(password, 4)", "@generate(password, 999)", "@generate(password, 32, x)", "@generate(port, 70000)", "@generate(uuid, 3)", "@generate(password"} {
		if _, ok, err := ParseGenerate(in); !ok || err == nil {
			t.Errorf("%s: ok=%v err=%v, want an error", in, ok, err)
		}
	}
	if _, ok, _ := ParseGenerate("plain"); ok {
		t.Error("plain text is not a keyword")
	}
	if _, err := (GenSpec{Kind: KindPort}).Value(); err == nil {
		t.Error("ports are chosen by the caller")
	}
}

func TestCoolifySpec(t *testing.T) {
	cases := map[string]string{
		"SERVICE_PASSWORD_DB":               "password, 32",
		"SERVICE_PASSWORD_64_DB":            "password, 64",
		"SERVICE_PASSWORDWITHSYMBOLS_64_DB": "password, 64, symbols",
		"SERVICE_USER_DB":                   "alnum, 16",
		"SERVICE_LOWERCASEUSER_DB":          "lowercaseuser",
		"SERVICE_BASE64_128_APP":            "alnum, 128",
		"SERVICE_REALBASE64_64_APP":         "base64, 64",
		"SERVICE_HEX_32_APP":                "hex, 16",
		"SERVICE_PORT_WEB":                  "port",
		"SERVICE_URL_APP_3000":              "url",
		"SERVICE_FQDN_APP":                  "fqdn",
	}
	for name, want := range cases {
		spec, ok := CoolifySpec(name)
		if !ok || spec.String() != want {
			t.Errorf("%s: %q ok=%v, want %q", name, spec.String(), ok, want)
		}
		if spec.Kind != KindPort && spec.Kind != "url" && spec.Kind != "fqdn" {
			if _, err := spec.Value(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	for _, name := range []string{"SERVICE_PASSWORD_", "POSTGRES_PASSWORD", "SERVICE_FOO_X"} {
		if _, ok := CoolifySpec(name); ok {
			t.Errorf("%s should not be a magic variable", name)
		}
	}
}
