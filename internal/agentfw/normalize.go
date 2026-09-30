package agentfw

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// invisibleChars are Unicode codepoints with no visible glyph used to evade pattern matching.
var invisibleChars = map[rune]bool{
	0x00AD: true, // soft hyphen
	0x200B: true, // zero-width space
	0x200C: true, // zero-width non-joiner
	0x200D: true, // zero-width joiner
	0x2060: true, // word joiner
	0xFEFF: true, // BOM / zero-width no-break space
	0x180E: true, // mongolian vowel separator
	0x034F: true, // combining grapheme joiner
}

// variationSelectors strips Unicode variation selector codepoints (U+FE00–FE0F, U+E0100–E01EF).
func isVariationSelector(r rune) bool {
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF)
}

// leetMap folds common leet substitutions to their base letter.
var leetMap = map[rune]rune{
	'0': 'o', '1': 'i', '3': 'e', '4': 'a',
	'5': 's', '7': 't', '@': 'a', '$': 's',
	'|': 'i', '!': 'i',
}

// homoglyphMap folds Cyrillic and Greek lookalikes to their Latin equivalents.
var homoglyphMap = map[rune]rune{
	// Cyrillic
	0x0430: 'a', 0x0435: 'e', 0x043E: 'o', 0x0440: 'p',
	0x0441: 'c', 0x0443: 'y', 0x0445: 'x', 0x0456: 'i',
	0x0410: 'A', 0x0415: 'E', 0x041E: 'O', 0x0420: 'P',
	0x0421: 'C', 0x0422: 'T', 0x0425: 'X',
	// Greek
	0x03B1: 'a', 0x03B5: 'e', 0x03BF: 'o', 0x03C1: 'p',
	0x0391: 'A', 0x0395: 'E', 0x039F: 'O', 0x03A1: 'P',
}

// NormalizeDLP applies normalization safe for credential patterns: NFKC,
// invisible char stripping, and homoglyph folding. No leet fold — digits
// are significant in keys and tokens.
func NormalizeDLP(text string) string {
	return normalize(text, false)
}

// Normalize applies the full normalization pipeline including leet folding.
// Use for injection/NL text scanning where digits-as-letters are evasion.
func Normalize(text string) string {
	return normalize(text, true)
}

func normalize(text string, leet bool) string {
	text = norm.NFKC.String(text)
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if invisibleChars[r] || isVariationSelector(r) {
			continue
		}
		if latin, ok := homoglyphMap[r]; ok {
			b.WriteRune(latin)
			continue
		}
		if leet && !unicode.IsLetter(r) && !unicode.IsSpace(r) {
			if latin, ok := leetMap[r]; ok {
				b.WriteRune(latin)
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
