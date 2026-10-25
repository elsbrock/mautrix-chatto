package connector

import "testing"

func TestCheckServerVersion(t *testing.T) {
	tests := map[string]bool{
		"v0.5.0-beta.21": true,
		"0.5.0-beta.21":  true,
		"v0.5.0-beta.22": true,
		"v0.5.0":         true,
		"v0.6.1":         true,
		"v0.5.0-beta.3":  false,
		"v0.4.9":         false,
		"0.4.0":          false,
		"dev":            true,
		"":               true,
	}
	for in, want := range tests {
		if got := CheckServerVersion(in); got != want {
			t.Errorf("CheckServerVersion(%q) = %v, want %v", in, got, want)
		}
	}
}
