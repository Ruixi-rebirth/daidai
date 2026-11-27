package core

import "testing"

func TestNormalizeDisplayName(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		userID string
		want   string
	}{
		{name: "normal", raw: "  月  ", userID: "6683CF02652D", want: "月"},
		{name: "zwj_emoji", raw: "👨‍👩‍👧", userID: "6683CF02652D", want: "👨‍👩‍👧"},
		{name: "invisible_times", raw: "\u2062", userID: "5AD8851C41E4FD2EDE5F33A7F97C69F8", want: "群成员#5AD885"},
		{name: "zwsp_and_space", raw: " \u200b\ufeff ", userID: "ABCDEF123456", want: "群成员#ABCDEF"},
		{name: "hangul_filler", raw: "\u3164", userID: "12345678", want: "群成员#123456"},
		{name: "braille_blank", raw: "\u2800", userID: "1234", want: "群成员#1234"},
		{name: "empty_both", raw: "\u2062", userID: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeDisplayName(tc.raw, tc.userID); got != tc.want {
				t.Fatalf("NormalizeDisplayName(%q, %q) = %q, want %q", tc.raw, tc.userID, got, tc.want)
			}
		})
	}
}
