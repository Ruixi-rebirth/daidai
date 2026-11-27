package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// HasVisibleText reports whether s contains at least one visible rune,
// treating whitespace, control/format characters (e.g. U+2062, U+200B,
// U+FEFF), and common blank filler codepoints (e.g. U+3164, U+2800) as
// invisible.
func HasVisibleText(s string) bool {
	for _, r := range s {
		if !isInvisibleDisplayRune(r) {
			return true
		}
	}
	return false
}

// NormalizeDisplayName trims a user-supplied display name and replaces
// empty or invisible-only names with a deterministic readable fallback
// ("群成员#<shortID>") so mentions, history, and avatar lookup agree.
func NormalizeDisplayName(name, userID string) string {
	if HasVisibleText(name) {
		return strings.TrimSpace(name)
	}
	uid := strings.TrimSpace(userID)
	if uid == "" {
		return ""
	}
	if len(uid) > 6 {
		uid = uid[:6]
	}
	return "群成员#" + uid
}

func isInvisibleDisplayRune(r rune) bool {
	if unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) {
		return true
	}
	switch r {
	case '\u034f', '\u115f', '\u1160', '\u17b4', '\u17b5', '\u180e', '\u2800', '\u3164', '\uffa0':
		return true
	default:
		return false
	}
}

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
