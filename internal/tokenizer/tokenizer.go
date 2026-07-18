package tokenizer

import (
	"encoding/json"
	"unicode/utf8"
)

// Counter is deliberately provider-independent. Compatible providers do not
// expose their tokenizer, so this estimate is conservative for mixed CJK/code.
type Counter struct{}

func (Counter) CountString(s string) int {
	runes := utf8.RuneCountInString(s)
	bytes := len(s)
	// CJK is usually close to one token per rune while ASCII prose/code is
	// commonly 3-4 bytes per token. Taking the larger estimate is safe.
	a := runes
	b := bytes/3 + 1
	if b > a {
		return b
	}
	return a + 1
}

func (c Counter) CountJSON(v any) int {
	b, _ := json.Marshal(v)
	return c.CountString(string(b))
}
