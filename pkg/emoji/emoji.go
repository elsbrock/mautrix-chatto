// Package emoji maps between Unicode emoji, which Matrix uses as reaction
// keys, and the reaction names that Chatto's API accepts, such as "thumbsup"
// or "thumbsup::skin-tone-4".
package emoji

import (
	"strings"
)

//go:generate node ../../tools/gen-emoji.mjs

type entry struct {
	unicode string
	names   []string
}

var (
	nameToUnicode = make(map[string]string, 4096)
	unicodeToName = make(map[string]string, 4096)
)

func init() {
	for _, e := range catalogue {
		key := normalize(e.unicode)
		if _, exists := unicodeToName[key]; !exists {
			unicodeToName[key] = e.names[0]
		}
		for _, name := range e.names {
			nameToUnicode[name] = e.unicode
		}
	}
}

const (
	variationSelector16 = "️"
	variationSelector15 = "︎"
)

// normalize strips presentation selectors so that "❤" and "❤️" resolve to the
// same entry. Matrix clients are inconsistent about including them.
func normalize(s string) string {
	s = strings.ReplaceAll(s, variationSelector16, "")
	return strings.ReplaceAll(s, variationSelector15, "")
}

func isSkinTone(r rune) bool {
	return r >= 0x1F3FB && r <= 0x1F3FF
}

func stripSkinTones(s string) string {
	return strings.Map(func(r rune) rune {
		if isSkinTone(r) {
			return -1
		}
		return r
	}, s)
}

// ToShortcode returns the Chatto reaction name for a Unicode emoji: the name
// Chatto's own frontend sends, so reactions from both sides count together.
// Skin tones are dropped only when Chatto has no variant of the emoji with
// that tone. The second return value is false when the emoji cannot be
// represented on Chatto at all.
func ToShortcode(unicode string) (string, bool) {
	unicode = strings.TrimSpace(unicode)
	if name, ok := unicodeToName[normalize(unicode)]; ok {
		return name, true
	}
	if name, ok := unicodeToName[normalize(stripSkinTones(unicode))]; ok {
		return name, true
	}
	// Some clients send ":shortcode:" as the reaction key. Aliases such as
	// "+1" map to the name Chatto's frontend uses, so they count together.
	if trimmed, ok := strings.CutPrefix(unicode, ":"); ok {
		if trimmed, ok = strings.CutSuffix(trimmed, ":"); ok {
			if emoji, known := nameToUnicode[trimmed]; known {
				return unicodeToName[normalize(emoji)], true
			}
		}
	}
	return "", false
}

// FromShortcode returns the Unicode emoji for a Chatto reaction name. Unknown
// names come back as ":name:" so that nothing is silently lost.
func FromShortcode(name string) string {
	if unicode, ok := nameToUnicode[name]; ok {
		return unicode
	}
	return ":" + name + ":"
}

// IsKnownShortcode reports whether Chatto will accept name as a reaction.
func IsKnownShortcode(name string) bool {
	_, ok := nameToUnicode[name]
	return ok
}
