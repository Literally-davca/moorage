package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// SemVer represents a parsed Semantic Version (major.minor.patch[-prerelease][+build]).
type SemVer struct {
	Raw   string
	Major int
	Minor int
	Patch int
	Pre   []string
}

// ParseVersion parses v (with optional leading "v") as SemVer 2.0.0.
// Non-semver strings such as "dev" return an error.
func ParseVersion(v string) (SemVer, error) {
	orig := strings.TrimSpace(v)
	s := strings.TrimPrefix(orig, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var prePart string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		prePart = s[i+1:]
		s = s[:i]
		if prePart == "" {
			return SemVer{}, fmt.Errorf("invalid version %q: empty pre-release", orig)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("invalid version %q: want major.minor.patch", orig)
	}
	maj, err := parseCorePart(parts[0])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid version %q: %w", orig, err)
	}
	min, err := parseCorePart(parts[1])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid version %q: %w", orig, err)
	}
	pat, err := parseCorePart(parts[2])
	if err != nil {
		return SemVer{}, fmt.Errorf("invalid version %q: %w", orig, err)
	}
	var pre []string
	if prePart != "" {
		pre = strings.Split(prePart, ".")
		for _, id := range pre {
			if id == "" {
				return SemVer{}, fmt.Errorf("invalid version %q: empty pre-release identifier", orig)
			}
		}
	}
	return SemVer{Raw: strings.TrimPrefix(orig, "v"), Major: maj, Minor: min, Patch: pat, Pre: pre}, nil
}

func parseCorePart(p string) (int, error) {
	if p == "" {
		return 0, fmt.Errorf("empty version component")
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("leading zero in %q", p)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad version number %q", p)
	}
	return n, nil
}

// IsPreRelease reports whether version string v has a SemVer pre-release suffix.
func IsPreRelease(v string) bool {
	sv, err := ParseVersion(v)
	if err != nil {
		return false
	}
	return len(sv.Pre) > 0
}

// CompareVersion compares two SemVer strings: -1 if a < b, 0 if a == b, +1 if a > b.
func CompareVersion(a, b string) (int, error) {
	sa, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	sb, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	return compareSemVer(sa, sb), nil
}

func compareSemVer(a, b SemVer) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	// Release version is strictly greater than any pre-release of the same core version.
	if len(a.Pre) == 0 && len(b.Pre) == 0 {
		return 0
	}
	if len(a.Pre) == 0 {
		return 1
	}
	if len(b.Pre) == 0 {
		return -1
	}
	n := min(len(a.Pre), len(b.Pre))
	for i := range n {
		if c := comparePreIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a.Pre), len(b.Pre))
}

func comparePreIdent(a, b string) int {
	if a == b {
		return 0
	}
	na, erra := strconv.Atoi(a)
	nb, errb := strconv.Atoi(b)
	aNum := erra == nil && !strings.HasPrefix(a, "-") && (len(a) == 1 || a[0] != '0')
	bNum := errb == nil && !strings.HasPrefix(b, "-") && (len(b) == 1 || b[0] != '0')
	switch {
	case aNum && bNum:
		return cmpInt(na, nb)
	case aNum && !bNum:
		return -1 // numeric identifiers always have lower precedence than non-numeric
	case !aNum && bNum:
		return 1
	default:
		if a < b {
			return -1
		}
		return 1
	}
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// IsNewer reports whether candidate > current according to SemVer.
// Non-semver current builds (such as "dev") return false and a parse error so dev
// builds are never silently replaced.
func IsNewer(candidate, current string) (bool, error) {
	c, err := CompareVersion(candidate, current)
	if err != nil {
		return false, err
	}
	return c > 0, nil
}

// SatisfiesMin checks whether current >= minVersion (when minVersion is non-empty).
func SatisfiesMin(current, minVersion string) (bool, error) {
	if strings.TrimSpace(minVersion) == "" {
		return true, nil
	}
	c, err := CompareVersion(current, minVersion)
	if err != nil {
		return false, err
	}
	return c >= 0, nil
}

// AssetNameFor returns the released binary filename for the given OS/architecture.
func AssetNameFor(goos, goarch string) (string, error) {
	if goos != "linux" {
		return "", fmt.Errorf("unsupported OS %q (only Linux updates are published)", goos)
	}
	switch goarch {
	case "amd64":
		return "moorage-linux-amd64", nil
	case "arm64":
		return "moorage-linux-arm64", nil
	case "arm":
		return "moorage-linux-armv7", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q", goarch)
	}
}

// PlatformFor returns the OCI platform key in images.json for the given OS/architecture.
func PlatformFor(goos, goarch string) (string, error) {
	if goos != "linux" {
		return "", fmt.Errorf("unsupported OS %q (only Linux images are published)", goos)
	}
	switch goarch {
	case "amd64":
		return "linux/amd64", nil
	case "arm64":
		return "linux/arm64", nil
	case "arm":
		return "linux/arm/v7", nil
	default:
		return "", fmt.Errorf("unsupported architecture %q", goarch)
	}
}
