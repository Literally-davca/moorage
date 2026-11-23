package api

import (
	"strings"
	"testing"
)

const addonsBase = `services:
  trivy:
    image: aquasec/trivy
    labels:
      moorage.addon: trivy-scanner
`

func TestAddonScaleDisable(t *testing.T) {
	out, changed, err := setAddonServiceDisabled(addonsBase, "trivy-scanner", true)
	if err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	doc, _ := parseComposeMap([]byte(out))
	svc := asMap(asMap(doc["services"])["trivy"])
	if svc["scale"] != 0 || svc["profiles"] != nil {
		t.Fatalf("disabled service: %v", svc)
	}
	out, _, _ = setAddonServiceDisabled(out, "trivy-scanner", false)
	doc, _ = parseComposeMap([]byte(out))
	if _, ok := asMap(asMap(doc["services"])["trivy"])["scale"]; ok {
		t.Fatalf("enabled service keeps scale: %s", out)
	}
}

func TestAddonMergeCollision(t *testing.T) {
	other := []byte("services:\n  trivy:\n    image: someone/else\n")
	if _, err := mergeAddonCompose(addonsBase, "restic-backup", other); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision not refused: %v", err)
	}
	// Reinstalling the same add-on replaces its own service.
	if _, err := mergeAddonCompose(addonsBase, "trivy-scanner", []byte("services:\n  trivy:\n    image: aquasec/trivy:latest\n")); err != nil {
		t.Fatalf("reinstall refused: %v", err)
	}
}
