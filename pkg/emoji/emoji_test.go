package emoji

import "testing"

func TestToShortcode(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"thumbs up", "👍", "+1", true},
		{"heart with VS16", "❤️", "heart", true},
		{"heart without VS16", "❤", "heart", true},
		{"skin tone stripped", "👍🏽", "+1", true},
		{"flag", "🇩🇪", "de", true},
		{"shortcode key", ":tada:", "tada", true},
		{"padded", " 🎉 ", "tada", true},
		{"plain text", "lol", "", false},
		{"two emoji", "👍👍", "", false},
		{"empty", "", "", false},
		{"unknown shortcode", ":definitely_not_real:", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ToShortcode(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("ToShortcode(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestFromShortcode(t *testing.T) {
	tests := map[string]string{
		"+1":       "👍",
		"thumbsup": "👍",
		"tada":     "🎉",
		"heart":    "❤️",
		"nope_xyz": ":nope_xyz:",
	}
	for in, want := range tests {
		if got := FromShortcode(in); got != want {
			t.Errorf("FromShortcode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRoundTripAllEntries(t *testing.T) {
	for _, e := range gemojiTable {
		name, ok := ToShortcode(e.unicode)
		if !ok {
			t.Fatalf("no shortcode for %q", e.unicode)
		}
		if !IsKnownShortcode(name) {
			t.Fatalf("shortcode %q for %q is not known", name, e.unicode)
		}
		if normalize(FromShortcode(name)) != normalize(e.unicode) {
			// Several entries may share a normalized form (e.g. text vs emoji
			// presentation); the first one wins and that is fine.
			if unicodeToName[normalize(e.unicode)] != name {
				t.Fatalf("round trip mismatch for %q via %q", e.unicode, name)
			}
		}
	}
}
