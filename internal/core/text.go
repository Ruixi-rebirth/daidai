package core

import (
	"strings"
	"unicode/utf8"
)

// AppendTextWithinRuneLimit preserves the appended note and shortens the base
// text when both cannot fit within one stored-message budget.
func AppendTextWithinRuneLimit(base, addition string, limit int, truncationMarker string) string {
	if limit <= 0 {
		return ""
	}
	if addition == "" {
		return TruncateRunes(base, limit, truncationMarker)
	}
	separator := ""
	if base != "" && !strings.HasSuffix(base, "\n") {
		separator = "\n"
	}
	suffix := separator + addition
	suffixRunes := utf8.RuneCountInString(suffix)
	if suffixRunes >= limit {
		return TruncateRunes(suffix, limit, truncationMarker)
	}
	return TruncateRunes(base, limit-suffixRunes, truncationMarker) + suffix
}

// LimitSentences keeps complete sentences within the rune and sentence limits.
// If the rune limit cuts through the first sentence, it returns an empty string
// instead of producing a misleading fragment.
func LimitSentences(text string, maxRunes, maxSentences int) string {
	if maxRunes <= 0 || maxSentences <= 0 {
		return ""
	}
	runes := []rune(text)
	limit := min(len(runes), maxRunes)
	end, sentences := 0, 0
	for index, current := range runes[:limit] {
		if !sentenceEnd(runes, index, current) {
			continue
		}
		sentences++
		end = index + 1
		if sentences == maxSentences {
			return string(runes[:end])
		}
	}
	if len(runes) <= maxRunes {
		return text
	}
	if end > 0 {
		return string(runes[:end])
	}
	return ""
}

func sentenceEnd(runes []rune, index int, current rune) bool {
	if current == '。' || current == '！' || current == '？' || current == '!' || current == '?' {
		return true
	}
	return current == '.' && index+1 < len(runes) && runes[index+1] == ' '
}
