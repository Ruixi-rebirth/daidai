package render

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var mdEngine = goldmark.New(
	goldmark.WithExtensions(
		extension.Table,
		extension.Strikethrough,
		extension.TaskList,
	),
)

// typstQuote 将任意 UTF-8 文本编码为严格符合 Typst 词法规范的字符串字面量
func typstQuote(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 4)
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&sb, `\u{%x}`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func listIndentDepth(n ast.Node) int {
	depth := 0
	for p := n.Parent(); p != nil; p = p.Parent() {
		if _, ok := p.(*ast.List); ok {
			depth++
		}
	}
	if depth > 0 {
		return depth - 1
	}
	return 0
}

// MarkdownToTypst 基于 Goldmark AST 语法树遍历将 Markdown 编译为原生 Typst AST 节点与字符串字面量
func MarkdownToTypst(source []byte) string {
	protected, mathList := extractAndProtectMath(source)
	doc := mdEngine.Parser().Parse(text.NewReader(protected))
	var buf bytes.Buffer

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := n.(type) {
		case *ast.Document:
			// 根节点，无需处理

		case *ast.Heading:
			if entering {
				buf.WriteString(strings.Repeat("=", node.Level) + " ")
			} else {
				buf.WriteString("\n\n")
			}

		case *ast.Paragraph:
			if !entering {
				if _, inList := node.Parent().(*ast.ListItem); inList {
					buf.WriteString("\n")
				} else {
					buf.WriteString("\n\n")
				}
			}

		case *extast.Table:
			if entering {
				cols := len(node.Alignments)
				if cols == 0 {
					for c := node.FirstChild(); c != nil; c = c.NextSibling() {
						if c.ChildCount() > cols {
							cols = c.ChildCount()
						}
					}
				}
				if cols == 0 {
					cols = 1
				}
				fmt.Fprintf(&buf, "#table(columns: %d, stroke: 0.6pt + rgb(\"#d0d7de\"), inset: 8pt,\n", cols)
			} else {
				buf.WriteString(")\n\n")
			}

		case *extast.TableHeader:
			// 容器节点，直接遍历子单元格

		case *extast.TableRow:
			// 容器节点，直接遍历子单元格

		case *extast.TableCell:
			_, inHeader := node.Parent().(*extast.TableHeader)
			if entering {
				var attrs []string
				if inHeader {
					attrs = append(attrs, `fill: rgb("#f6f8fa")`)
				}
				switch node.Alignment {
				case extast.AlignRight:
					attrs = append(attrs, `align: right`)
				case extast.AlignCenter:
					attrs = append(attrs, `align: center`)
				case extast.AlignLeft:
					attrs = append(attrs, `align: left`)
				}

				if len(attrs) > 0 {
					buf.WriteString(fmt.Sprintf("  table.cell(%s)[", strings.Join(attrs, ", ")))
				} else {
					buf.WriteString("  [")
				}
				if inHeader {
					buf.WriteString("#strong[")
				}
			} else {
				if inHeader {
					buf.WriteString("]")
				}
				buf.WriteString("],\n")
			}

		case *extast.Strikethrough:
			if entering {
				buf.WriteString("#strike[")
			} else {
				buf.WriteString("]")
			}

		case *ast.TextBlock:
			if !entering {
				buf.WriteString("\n")
			}

		case *ast.Blockquote:
			if entering {
				buf.WriteString(`#block(stroke: (left: 3pt + rgb("#0969da")), inset: (left: 10pt, y: 5pt), fill: rgb("#f6f8fa"), radius: (right: 4pt))[`)
			} else {
				buf.WriteString("]\n\n")
			}

		case *ast.FencedCodeBlock:
			if entering {
				info := ""
				if node.Info != nil {
					info = strings.TrimSpace(string(node.Info.Value(protected)))
				}
				var codeBuf bytes.Buffer
				lines := node.Lines()
				for i := 0; i < lines.Len(); i++ {
					line := lines.At(i)
					codeBuf.Write(line.Value(protected))
				}
				codeText := strings.TrimRight(codeBuf.String(), "\r\n")
				if info != "" {
					fmt.Fprintf(&buf, "#raw(block: true, lang: %s, %s)\n\n", typstQuote(info), typstQuote(codeText))
				} else {
					fmt.Fprintf(&buf, "#raw(block: true, %s)\n\n", typstQuote(codeText))
				}
				return ast.WalkSkipChildren, nil
			}

		case *ast.CodeBlock:
			if entering {
				var codeBuf bytes.Buffer
				lines := node.Lines()
				for i := 0; i < lines.Len(); i++ {
					line := lines.At(i)
					codeBuf.Write(line.Value(protected))
				}
				codeText := strings.TrimRight(codeBuf.String(), "\r\n")
				fmt.Fprintf(&buf, "#raw(block: true, %s)\n\n", typstQuote(codeText))
				return ast.WalkSkipChildren, nil
			}

		case *ast.CodeSpan:
			if entering {
				var spanBuf bytes.Buffer
				for c := node.FirstChild(); c != nil; c = c.NextSibling() {
					if t, ok := c.(*ast.Text); ok {
						spanBuf.Write(t.Segment.Value(protected))
					}
				}
				fmt.Fprintf(&buf, "#raw(block: false, %s)", typstQuote(spanBuf.String()))
				return ast.WalkSkipChildren, nil
			}

		case *ast.List:
			if !entering && listIndentDepth(node) == 0 {
				buf.WriteString("\n")
			}

		case *ast.ListItem:
			if entering {
				indent := strings.Repeat("  ", listIndentDepth(node))
				parent := node.Parent()
				if list, ok := parent.(*ast.List); ok && list.IsOrdered() {
					buf.WriteString(indent + "+ ")
				} else {
					buf.WriteString(indent + "- ")
				}
			}

		case *ast.ThematicBreak:
			if entering {
				buf.WriteString(`#line(length: 100%, stroke: 0.8pt + rgb("#eaeef2"))` + "\n\n")
			}

		case *ast.Emphasis:
			if entering {
				if node.Level >= 2 {
					buf.WriteString("#strong[")
				} else {
					buf.WriteString("#emph[")
				}
			} else {
				buf.WriteString("]")
			}

		case *ast.Image:
			if entering {
				var altBuf bytes.Buffer
				for c := node.FirstChild(); c != nil; c = c.NextSibling() {
					if t, ok := c.(*ast.Text); ok {
						altBuf.Write(t.Segment.Value(protected))
					}
				}
				alt := strings.TrimSpace(altBuf.String())
				if alt == "" {
					alt = "图片"
				}
				buf.WriteString("#" + typstQuote("[图片: "+alt+"]"))
				return ast.WalkSkipChildren, nil
			}

		case *ast.Link:
			if entering {
				dest := string(node.Destination)
				fmt.Fprintf(&buf, "#link(%s)[", typstQuote(dest))
			} else {
				buf.WriteString("]")
			}

		case *ast.AutoLink:
			if entering {
				url := string(node.URL(protected))
				label := string(node.Label(protected))
				fmt.Fprintf(&buf, "#link(%s)[#%s]", typstQuote(url), typstQuote(label))
			}

		case *ast.Text:
			if entering {
				val := string(node.Segment.Value(protected))
				if val != "" {
					renderTextOrMath(val, mathList, &buf)
				}
				if node.HardLineBreak() {
					buf.WriteString(" \\\n")
				} else if node.SoftLineBreak() {
					buf.WriteString("\n")
				}
			}

		case *ast.String:
			if entering {
				val := string(node.Value)
				if val != "" {
					renderTextOrMath(val, mathList, &buf)
				}
			}
		}

		return ast.WalkContinue, nil
	})

	return strings.TrimSpace(buf.String())
}

type mathToken struct {
	IsBlock bool
	Content string
}

var mathPlaceholderRegex = regexp.MustCompile(`DAIDAITYPSTMATHPLACEHOLDERX(\d+)XX`)

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
	if len(mathList) == 0 || !strings.Contains(s, "DAIDAITYPSTMATHPLACEHOLDERX") {
		buf.WriteString("#" + typstQuote(s))
		return
	}

	matches := mathPlaceholderRegex.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		buf.WriteString("#" + typstQuote(s))
		return
	}

	lastIdx := 0
	for _, m := range matches {
		if m[0] > lastIdx {
			textPart := s[lastIdx:m[0]]
			if textPart != "" {
				buf.WriteString("#" + typstQuote(textPart))
			}
		}
		idStr := s[m[2]:m[3]]
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
		lastIdx = m[1]
	}

	if lastIdx < len(s) {
		textPart := s[lastIdx:]
		if textPart != "" {
			buf.WriteString("#" + typstQuote(textPart))
		}
	}
}
