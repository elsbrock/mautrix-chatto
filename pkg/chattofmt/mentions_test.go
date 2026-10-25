package chattofmt

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// TestExtractMentionsUpstreamCases runs the extraction cases shared by
// Chatto's server and frontend, so the bridge agrees with both.
func TestExtractMentionsUpstreamCases(t *testing.T) {
	data, err := os.ReadFile("testdata/mentions_extraction.json")
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Cases []struct {
			Name     string   `json:"name"`
			Body     string   `json:"body"`
			Mentions []string `json:"mentions"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &suite); err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range suite.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ExtractMentions(tc.Body)
			if len(got) == 0 && len(tc.Mentions) == 0 {
				return
			}
			if !slices.Equal(got, tc.Mentions) {
				t.Fatalf("ExtractMentions(%q) = %q, want %q", tc.Body, got, tc.Mentions)
			}
		})
	}
}

func TestIsValidHandle(t *testing.T) {
	tests := map[string]bool{
		"alice":        true,
		"first.last":   true,
		"under_score":  true,
		"with-dash":    true,
		"trailingdot.": false,
		"":             false,
		"has space":    false,
		"ünicode":      false,
	}
	for in, want := range tests {
		if got := IsValidHandle(in); got != want {
			t.Errorf("IsValidHandle(%q) = %v, want %v", in, got, want)
		}
	}
}
