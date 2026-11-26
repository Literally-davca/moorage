package notify

import "testing"

func TestChangesShowOnlyInDetailed(t *testing.T) {
	m := Message{Status: "down", Title: "DOWN: web", Changes: []string{"14:02 UTC Changed compose.yaml (admin)"}}
	has := func(v View) bool {
		for _, f := range v.Fields {
			if f.Name == "Changed before" {
				return true
			}
		}
		return false
	}
	if !has(BuildView(Config{Preset: PresetDetailed}, m)) {
		t.Fatal("Detailed must show what changed before the outage")
	}
	if has(BuildView(Config{Preset: PresetStandard}, m)) || has(BuildView(Config{Preset: PresetMinimal}, m)) {
		t.Fatal("Standard and Minimal leave it out")
	}
}
