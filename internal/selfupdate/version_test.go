package selfupdate

import "testing"

func TestVersionCompareAndCompatGates(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.2.0", "1.1.9", 1},
		{"1.2.0", "v1.2.1", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.1", 1},
		{"1.0.0-alpha.beta", "1.0.0-alpha.1", 1},
		{"0.0.2-test", "0.0.1-test", 1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, tc := range cases {
		got, err := CompareVersion(tc.a, tc.b)
		if err != nil {
			t.Fatalf("CompareVersion(%q, %q) err=%v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Errorf("CompareVersion(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}

	if _, err := IsNewer("1.0.0", "dev"); err == nil {
		t.Fatal("expected error comparing against dev version")
	}
	if newer, err := IsNewer("1.0.0", "1.1.0"); err != nil || newer {
		t.Fatalf("downgrade check failed: newer=%v err=%v", newer, err)
	}
	if newer, err := IsNewer("1.2.0", "1.1.0"); err != nil || !newer {
		t.Fatalf("upgrade check failed: newer=%v err=%v", newer, err)
	}

	if ok, err := SatisfiesMin("1.2.0", "1.2.0"); err != nil || !ok {
		t.Fatalf("expected min satisfied")
	}
	if ok, err := SatisfiesMin("1.1.0", "1.2.0"); err != nil || ok {
		t.Fatalf("expected min_hub gate failure when current < min")
	}
	if ok, err := SatisfiesMin("1.1.0", ""); err != nil || !ok {
		t.Fatalf("expected empty min to be satisfied")
	}
	if !IsPreRelease("1.0.0-rc.1") || IsPreRelease("1.0.0") {
		t.Fatalf("IsPreRelease mismatch")
	}
	if n, err := AssetNameFor("linux", "arm"); err != nil || n != "moorage-linux-armv7" {
		t.Fatalf("AssetNameFor linux/arm = %q, err=%v", n, err)
	}
	if p, err := PlatformFor("linux", "arm64"); err != nil || p != "linux/arm64" {
		t.Fatalf("PlatformFor linux/arm64 = %q, err=%v", p, err)
	}
}
