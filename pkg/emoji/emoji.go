// Package emoji maps between Unicode emoji, which Matrix uses as reaction
// keys, and the gemoji shortcode names that Chatto's reaction API accepts.
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
	nameToUnicode = make(map[string]string, 2048)
	unicodeToName = make(map[string]string, 2048)
)

func init() {
	for _, e := range gemojiTable {
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

// ToShortcode returns the Chatto reaction name for a Unicode emoji. Skin tone
// modifiers are dropped when no exact entry exists, because Chatto's dataset
// has no skin tone variants. The second return value is false when the emoji
// cannot be represented on Chatto at all.
func ToShortcode(unicode string) (string, bool) {
	unicode = strings.TrimSpace(unicode)
	if name, ok := unicodeToName[normalize(unicode)]; ok {
		return name, true
	}
	if name, ok := unicodeToName[normalize(stripSkinTones(unicode))]; ok {
		return name, true
	}
	// Some clients send ":shortcode:" as the reaction key.
	if trimmed, ok := strings.CutPrefix(unicode, ":"); ok {
		if trimmed, ok = strings.CutSuffix(trimmed, ":"); ok {
			if _, known := nameToUnicode[trimmed]; known {
				return trimmed, true
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
