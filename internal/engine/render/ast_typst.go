package render

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

func parseTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

func isTableDelimiter(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") || !strings.Contains(trimmed, "-") {
		return false
	}
	for _, r := range trimmed {
		if r != '|' && r != '-' && r != ':' && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func tryRenderMarkdownTable(node *ast.Paragraph, source []byte, buf *bytes.Buffer) bool {
	lines := node.Lines()
	if lines.Len() < 2 {
		return false
	}
	rawLines := make([]string, 0, lines.Len())
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		rawLines = append(rawLines, strings.TrimRight(string(seg.Value(source)), "\r\n"))
	}
	if !strings.Contains(rawLines[0], "|") || !isTableDelimiter(rawLines[1]) {
		return false
	}
	headers := parseTableRow(rawLines[0])
	cols := len(headers)
	if cols == 0 {
		return false
	}
	buf.WriteString(fmt.Sprintf("#table(columns: %d, stroke: 0.6pt + rgb(\"#d0d7de\"), inset: 8pt,\n", cols))
	for _, h := range headers {
		buf.WriteString(fmt.Sprintf("  table.cell(fill: rgb(\"#f6f8fa\"))[#strong[#%s]],\n", typstQuote(h)))
	}
	for _, rowLine := range rawLines[2:] {
		if !strings.Contains(rowLine, "|") {
			continue
		}
		cells := parseTableRow(rowLine)
		for i := 0; i < cols; i++ {
			val := ""
			if i < len(cells) {
				val = cells[i]
			}
			buf.WriteString(fmt.Sprintf("  [#%s],\n", typstQuote(val)))
		}
	}
	buf.WriteString(")\n\n")
	return true
}

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
	doc := goldmark.DefaultParser().Parse(text.NewReader(source))
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
			if entering {
				if tryRenderMarkdownTable(node, source, &buf) {
					return ast.WalkSkipChildren, nil
				}
			} else {
				if _, inList := node.Parent().(*ast.ListItem); inList {
					buf.WriteString("\n")
				} else {
					buf.WriteString("\n\n")
				}
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
					info = strings.TrimSpace(string(node.Info.Text(source)))
				}
				var codeBuf bytes.Buffer
				lines := node.Lines()
				for i := 0; i < lines.Len(); i++ {
					line := lines.At(i)
					codeBuf.Write(line.Value(source))
				}
				codeText := strings.TrimRight(codeBuf.String(), "\r\n")
				if info != "" {
					buf.WriteString(fmt.Sprintf("#raw(block: true, lang: %s, %s)\n\n", typstQuote(info), typstQuote(codeText)))
				} else {
					buf.WriteString(fmt.Sprintf("#raw(block: true, %s)\n\n", typstQuote(codeText)))
				}
				return ast.WalkSkipChildren, nil
			}

		case *ast.CodeBlock:
			if entering {
				var codeBuf bytes.Buffer
				lines := node.Lines()
				for i := 0; i < lines.Len(); i++ {
					line := lines.At(i)
					codeBuf.Write(line.Value(source))
				}
				codeText := strings.TrimRight(codeBuf.String(), "\r\n")
				buf.WriteString(fmt.Sprintf("#raw(block: true, %s)\n\n", typstQuote(codeText)))
				return ast.WalkSkipChildren, nil
			}

		case *ast.CodeSpan:
			if entering {
				var spanBuf bytes.Buffer
				for c := node.FirstChild(); c != nil; c = c.NextSibling() {
					if t, ok := c.(*ast.Text); ok {
						spanBuf.Write(t.Segment.Value(source))
					}
				}
				buf.WriteString(fmt.Sprintf("#raw(block: false, %s)", typstQuote(spanBuf.String())))
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
				dest := strings.TrimSpace(string(node.Destination))
				if dest != "" {
					if absPath, err := filepath.Abs(dest); err == nil {
						if info, statErr := os.Stat(absPath); statErr == nil && !info.IsDir() {
							buf.WriteString(fmt.Sprintf("#image(%s, width: 100%%)", typstQuote(absPath)))
							return ast.WalkSkipChildren, nil
						}
					}
				}
				var altBuf bytes.Buffer
				for c := node.FirstChild(); c != nil; c = c.NextSibling() {
					if t, ok := c.(*ast.Text); ok {
						altBuf.Write(t.Segment.Value(source))
					}
				}
				alt := strings.TrimSpace(altBuf.String())
				if alt == "" {
					alt = dest
				}
				buf.WriteString("#" + typstQuote("[图片: "+alt+"]"))
				return ast.WalkSkipChildren, nil
			}

		case *ast.Link:
			if entering {
				dest := string(node.Destination)
				buf.WriteString(fmt.Sprintf("#link(%s)[", typstQuote(dest)))
			} else {
				buf.WriteString("]")
			}

		case *ast.AutoLink:
			if entering {
				url := string(node.URL(source))
				label := string(node.Label(source))
				buf.WriteString(fmt.Sprintf("#link(%s)[#%s]", typstQuote(url), typstQuote(label)))
			}

		case *ast.Text:
			if entering {
				val := string(node.Segment.Value(source))
				if val != "" {
					buf.WriteString("#" + typstQuote(val))
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
					buf.WriteString("#" + typstQuote(val))
				}
			}
		}

		return ast.WalkContinue, nil
	})

	return strings.TrimSpace(buf.String())
}
