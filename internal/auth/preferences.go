package auth

import (
	"encoding/json"
	"strings"
)

// UserPreferences stores per-user UX mode, editor defaults, and manual add-on overrides.
type UserPreferences struct {
	Mode            string          `json:"mode,omitempty"` // "" | "beginner" | "advanced" | "custom"
	OnboardingDone  bool            `json:"onboardingDone"`
	ComposeView     string          `json:"composeView,omitempty"` // "form" | "yaml-comments" | "yaml-clean"
	EnvView         string          `json:"envView,omitempty"`     // "vars" | "raw"
	LinterShowTips  bool            `json:"linterShowTips"`
	NewStackDefault string          `json:"newStackDefault,omitempty"` // "templates" | "blank"
	GuidesMode      string          `json:"guidesMode,omitempty"`      // "extended" | "compact"
	ConfirmActions  string          `json:"confirmActions,omitempty"`  // "all" | "destructive"
	ManualOverrides map[string]bool `json:"manualOverrides,omitempty"`
}

// PresetPreferences returns the built-in defaults for "beginner" or "advanced".
func PresetPreferences(mode string) UserPreferences {
	if mode == "beginner" {
		return UserPreferences{
			Mode:            "beginner",
			OnboardingDone:  true,
			ComposeView:     "form",
			EnvView:         "vars",
			LinterShowTips:  true,
			NewStackDefault: "templates",
			GuidesMode:      "extended",
			ConfirmActions:  "all",
		}
	}
	return UserPreferences{
		Mode:            "advanced",
		OnboardingDone:  true,
		ComposeView:     "yaml-comments",
		EnvView:         "raw",
		LinterShowTips:  false,
		NewStackDefault: "blank",
		GuidesMode:      "compact",
		ConfirmActions:  "destructive",
	}
}

// ParsePreferences decodes stored JSON; empty string yields OnboardingDone=false with advanced-compatible defaults.
func ParsePreferences(raw string) *UserPreferences {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &UserPreferences{OnboardingDone: false}
	}
	var p UserPreferences
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return &UserPreferences{OnboardingDone: false}
	}
	n := p.Normalize()
	return &n
}

// Normalize validates enum fields and automatically detects "custom" when fields diverge from the named preset.
func (p UserPreferences) Normalize() UserPreferences {
	switch p.Mode {
	case "beginner", "advanced", "custom":
	default:
		p.Mode = ""
	}
	if p.Mode == "beginner" || p.Mode == "advanced" {
		// If none of the individual fields are set, fill them from the preset.
		if p.ComposeView == "" && p.EnvView == "" && p.NewStackDefault == "" && p.GuidesMode == "" && p.ConfirmActions == "" {
			preset := PresetPreferences(p.Mode)
			preset.OnboardingDone = p.OnboardingDone
			preset.ManualOverrides = cleanOverrides(p.ManualOverrides)
			return preset
		}
	}
	switch p.ComposeView {
	case "form", "yaml-comments", "yaml-clean":
	default:
		p.ComposeView = "yaml-comments"
	}
	switch p.EnvView {
	case "vars", "raw":
	default:
		p.EnvView = "vars"
	}
	switch p.NewStackDefault {
	case "templates", "blank":
	default:
		p.NewStackDefault = "templates"
	}
	switch p.GuidesMode {
	case "extended", "compact":
	default:
		p.GuidesMode = "extended"
	}
	switch p.ConfirmActions {
	case "all", "destructive":
	default:
		p.ConfirmActions = "destructive"
	}
	p.ManualOverrides = cleanOverrides(p.ManualOverrides)
	if p.Mode == "" && p.OnboardingDone {
		p.Mode = "custom"
	}
	return p
}

func cleanOverrides(in map[string]bool) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := map[string]bool{}
	for k, v := range in {
		k = strings.TrimSpace(k)
		if k != "" && v {
			out[k] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
