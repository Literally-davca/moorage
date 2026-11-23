package engine

import "testing"

func TestMatchesContainerList(t *testing.T) {
	c := Container{ID: "abc", Name: "web-app-1", Service: "app"}
	for want, ok := range map[string]bool{
		"":                true,
		"app":             true,
		"web-app-1":       true,
		"abc":             true,
		"db":              false,
		"db, app":         true,
		"db,web-app-1,x":  true,
		"db,cache":        false,
		" , ":             false,
		"app-1,web-app-2": false,
	} {
		if got := matchesContainer(c, want); got != ok {
			t.Errorf("matchesContainer(%q) = %v, want %v", want, got, ok)
		}
	}
}
