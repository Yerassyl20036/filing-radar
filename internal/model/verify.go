package model

import (
	"strings"
	"unicode"
)

// Verify says whether the model's quote really appears in the filing. A score
// whose quote can't be found is dropped: the model may have paraphrased, or
// made it up, and either way the reason can't be checked.
//
// The match ignores case, spacing and typographic differences (curly vs
// straight quotes, dashes, non-breaking spaces), because HTML-to-text and the
// model both change those freely. It needs at least four words, so a quote
// like "the Company" can't pass by accident.
func Verify(quote, text string) bool {
	q := normalize(quote)
	if len(strings.Fields(q)) < 4 {
		return false
	}
	return strings.Contains(normalize(text), q)
}

var typographic = strings.NewReplacer(
	"‘", "'", "’", "'", "“", `"`, "”", `"`,
	"–", "-", "—", "-", "−", "-", " ", " ", "…", "...",
)

func normalize(s string) string {
	s = typographic.Replace(strings.ToLower(s))
	// Collapse every run of whitespace to one space.
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	// Trailing punctuation the model may add or drop.
	return strings.Trim(b.String(), ` .,;:"'`)
}
