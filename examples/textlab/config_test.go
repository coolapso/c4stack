package main

import "testing"

// Only a local Textlab answers any host: production's must refuse the others.
func TestLocal(t *testing.T) {
	for url, want := range map[string]bool{
		"http://localhost:8082":        true,
		"http://127.0.0.1:8082":        true,
		"http://[::1]:8082":            true,
		"https://textlab.example.com":  false,
		"https://local.example.com":    false,
		"http://localhost.example.com": false,
		"":                             false,
	} {
		if got := (config{PublicURL: url}).local(); got != want {
			t.Errorf("local() for %q = %v, want %v", url, got, want)
		}
	}
}
