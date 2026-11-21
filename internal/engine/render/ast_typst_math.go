package render

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type mathToken struct {
	IsBlock bool
	Content string
}

const mathPlaceholderPrefix = "DAIDAITYPSTMATHPLACEHOLDERX"

// extractAndProtectMath 在 Goldmark AST 解析前保护 LaTeX 数学公式，防止其内部的下划线、星号或感叹号被 Markdown 解释器打碎
func extractAndProtectMath(src []byte) ([]byte, []mathToken) {
	var mathList []mathToken
	var out bytes.Buffer
	out.Grow(len(src))

	n := len(src)
	i := 0
	last := 0

	for i < n {
		// Fenced code block ``` or ~~~
		if (i+2 < n && src[i] == '`' && src[i+1] == '`' && src[i+2] == '`') ||
			(i+2 < n && src[i] == '~' && src[i+1] == '~' && src[i+2] == '~') {
			fence := src[i : i+3]
			end := bytes.Index(src[i+3:], fence)
			if end != -1 {
				i = i + 3 + end + 3
				continue
			}
		}

		// Inline code `
		if src[i] == '`' {
			end := bytes.IndexByte(src[i+1:], '`')
			if end != -1 {
				i = i + 1 + end + 1
				continue
			}
		}

		// Block math $$...$$
		if i+1 < n && src[i] == '$' && src[i+1] == '$' {
			if i > 0 && src[i-1] == '\\' {
				i += 2
				continue
			}
			end := bytes.Index(src[i+2:], []byte("$$"))
			if end != -1 {
				out.Write(src[last:i])
				idx := len(mathList)
				mathList = append(mathList, mathToken{
					IsBlock: true,
					Content: string(src[i+2 : i+2+end]),
				})
				fmt.Fprintf(&out, "DAIDAITYPSTMATHPLACEHOLDERX%dXX", idx)
				i = i + 2 + end + 2
				last = i
				continue
			}
		}

		// Inline math $...$
		if src[i] == '$' {
			if i > 0 && src[i-1] == '\\' {
				i++
				continue
			}
			// Inline math condition: not followed by space or digit (e.g. $100)
			if i+1 < n && !unicode.IsSpace(rune(src[i+1])) && (src[i+1] < '0' || src[i+1] > '9') {
				closeIdx := -1
				for j := i + 1; j < n; j++ {
					if src[j] == '$' && src[j-1] != '\\' {
						if !unicode.IsSpace(rune(src[j-1])) {
							closeIdx = j
							break
						}
					}
					if src[j] == '\n' {
						break
					}
				}
				if closeIdx != -1 {
					out.Write(src[last:i])
					idx := len(mathList)
					mathList = append(mathList, mathToken{
						IsBlock: false,
						Content: string(src[i+1 : closeIdx]),
					})
					fmt.Fprintf(&out, "DAIDAITYPSTMATHPLACEHOLDERX%dXX", idx)
					i = closeIdx + 1
					last = i
					continue
				}
			}
		}

		i++
	}

	out.Write(src[last:])
	return out.Bytes(), mathList
}

// renderTextOrMath 将文本和 LaTeX 数学公式占位符分离并渲染为 Typst
func renderTextOrMath(s string, mathList []mathToken, buf *bytes.Buffer) {
	if s == "" {
		return
	}
	if len(mathList) == 0 || !strings.Contains(s, mathPlaceholderPrefix) {
		buf.WriteString("#" + typstQuote(s))
		return
	}

	if _, _, _, _, ok := nextMathPlaceholder(s, 0); !ok {
		buf.WriteString("#" + typstQuote(s))
		return
	}

	lastIdx := 0
	for {
		matchStart, matchEnd, digitsStart, digitsEnd, ok := nextMathPlaceholder(s, lastIdx)
		if !ok {
			break
		}
		if matchStart > lastIdx {
			textPart := s[lastIdx:matchStart]
			if textPart != "" {
				buf.WriteString("#" + typstQuote(textPart))
			}
		}
		idStr := s[digitsStart:digitsEnd]
		id, err := strconv.Atoi(idStr)
		if err == nil && id >= 0 && id < len(mathList) {
			mToken := mathList[id]
			typstMath := LaTeXToTypstMath(mToken.Content)
			if typstMath != "" {
				if mToken.IsBlock {
					buf.WriteString("\n$ " + typstMath + " $\n")
				} else {
					buf.WriteString(" $" + typstMath + "$ ")
				}
			}
		}
		lastIdx = matchEnd
	}

	if lastIdx < len(s) {
		textPart := s[lastIdx:]
		if textPart != "" {
			buf.WriteString("#" + typstQuote(textPart))
		}
	}
}

func nextMathPlaceholder(s string, from int) (matchStart, matchEnd, digitsStart, digitsEnd int, ok bool) {
	searchFrom := from
	for searchFrom < len(s) {
		relative := strings.Index(s[searchFrom:], mathPlaceholderPrefix)
		if relative < 0 {
			return 0, 0, 0, 0, false
		}
		matchStart = searchFrom + relative
		digitsStart = matchStart + len(mathPlaceholderPrefix)
		digitsEnd = digitsStart
		for digitsEnd < len(s) && s[digitsEnd] >= '0' && s[digitsEnd] <= '9' {
			digitsEnd++
		}
		if digitsEnd > digitsStart && strings.HasPrefix(s[digitsEnd:], "XX") {
			return matchStart, digitsEnd + 2, digitsStart, digitsEnd, true
		}
		searchFrom = matchStart + len(mathPlaceholderPrefix)
	}
	return 0, 0, 0, 0, false
}
