package telegram

import (
	"bytes"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type textEntity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
}
type formattedText struct {
	Text     string
	Entities []textEntity
}
type textFormatter struct {
	strings.Builder
	units    int
	entities []textEntity
	source   []byte
}

func (f *textFormatter) write(s string) { f.WriteString(s); f.units += utf16Length(s) }
func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
func (f *textFormatter) children(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		f.node(c)
	}
}
func (f *textFormatter) span(n ast.Node, kind, link, language string) {
	start := f.units
	f.children(n)
	if f.units > start {
		f.entities = append(f.entities, textEntity{Type: kind, Offset: start, Length: f.units - start, URL: link, Language: language})
	}
}
func safeTextLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host != "" {
			return raw
		}
	case "mailto":
		return raw
	}
	return ""
}
func (f *textFormatter) node(n ast.Node) {
	switch n := n.(type) {
	case *ast.Text:
		value := n.Value(f.source)
		if !n.IsRaw() {
			value = util.UnescapePunctuations(value)
			value = []byte(html.UnescapeString(string(value)))
		}
		f.write(string(value))
		if n.SoftLineBreak() || n.HardLineBreak() {
			f.write("\n")
		}
	case *ast.String:
		f.write(string(n.Value))
	case *ast.Emphasis:
		kind := "italic"
		if n.Level == 2 {
			kind = "bold"
		}
		f.span(n, kind, "", "")
	case *ast.CodeSpan:
		f.span(n, "code", "", "")
	case *ast.Link:
		if link := safeTextLink(string(n.Destination)); link != "" {
			f.span(n, "text_link", link, "")
		} else {
			f.children(n)
		}
	case *ast.AutoLink:
		label := string(n.Label(f.source))
		link := safeTextLink(string(n.URL(f.source)))
		start := f.units
		f.write(label)
		if link != "" {
			f.entities = append(f.entities, textEntity{Type: "text_link", Offset: start, Length: f.units - start, URL: link})
		}
	case *ast.Image:
		f.children(n)
	case *ast.Heading:
		f.span(n, "bold", "", "")
		f.write("\n\n")
	case *ast.Paragraph:
		f.children(n)
		f.write("\n\n")
	case *ast.TextBlock:
		f.children(n)
		f.write("\n")
	case *ast.ListItem:
		prefix := "• "
		if list, ok := n.Parent().(*ast.List); ok && list.IsOrdered() {
			index := list.Start
			for p := n.PreviousSibling(); p != nil; p = p.PreviousSibling() {
				index++
			}
			prefix = strconv.Itoa(index) + ". "
		}
		f.write(prefix)
		f.children(n)
	case *ast.Blockquote:
		start := f.units
		f.children(n)
		nested := false
		for p := n.Parent(); p != nil; p = p.Parent() {
			if _, ok := p.(*ast.Blockquote); ok {
				nested = true
			}
		}
		if !nested && f.units > start {
			f.entities = append(f.entities, textEntity{Type: "blockquote", Offset: start, Length: f.units - start})
		}
	case *ast.FencedCodeBlock:
		f.code(n.Lines(), string(n.Language(f.source)))
	case *ast.CodeBlock:
		f.code(n.Lines(), "")
	case *ast.ThematicBreak:
		f.write("———\n")
	case *ast.RawHTML:
		for i := 0; i < n.Segments.Len(); i++ {
			segment := n.Segments.At(i)
			f.write(string(segment.Value(f.source)))
		}
	case *ast.HTMLBlock:
		for i := 0; i < n.Lines().Len(); i++ {
			segment := n.Lines().At(i)
			f.write(string(segment.Value(f.source)))
		}
	default:
		f.children(n)
	}
}
func (f *textFormatter) code(lines *text.Segments, language string) {
	var b bytes.Buffer
	for i := 0; i < lines.Len(); i++ {
		segment := lines.At(i)
		b.Write(segment.Value(f.source))
	}
	value := strings.TrimSuffix(b.String(), "\n")
	start := f.units
	f.write(value)
	if f.units > start {
		f.entities = append(f.entities, textEntity{Type: "pre", Offset: start, Length: f.units - start, Language: language})
	}
	f.write("\n\n")
}
func formatMarkdown(markdown string) formattedText {
	source := []byte(markdown)
	f := &textFormatter{source: source}
	f.node(goldmark.DefaultParser().Parse(text.NewReader(source)))
	result := formattedText{Text: strings.TrimRight(f.String(), "\n")}
	// Telegram disallows formatting entities overlapping code/pre entities.
	for _, entity := range f.entities {
		entity.Length = min(entity.Offset+entity.Length, utf16Length(result.Text)) - entity.Offset
		if entity.Length <= 0 {
			continue
		}
		pieces := []textEntity{entity}
		if entity.Type == "bold" || entity.Type == "italic" || entity.Type == "text_link" {
			for _, code := range f.entities {
				if code.Type != "code" && code.Type != "pre" {
					continue
				}
				var next []textEntity
				for _, piece := range pieces {
					end := piece.Offset + piece.Length
					codeEnd := code.Offset + code.Length
					if code.Offset >= end || codeEnd <= piece.Offset {
						next = append(next, piece)
						continue
					}
					if piece.Offset < code.Offset {
						left := piece
						left.Length = code.Offset - piece.Offset
						next = append(next, left)
					}
					if codeEnd < end {
						right := piece
						right.Offset = codeEnd
						right.Length = end - codeEnd
						next = append(next, right)
					}
				}
				pieces = next
			}
		}
		result.Entities = append(result.Entities, pieces...)
	}
	sort.SliceStable(result.Entities, func(i, j int) bool {
		if result.Entities[i].Offset == result.Entities[j].Offset {
			return result.Entities[i].Length > result.Entities[j].Length
		}
		return result.Entities[i].Offset < result.Entities[j].Offset
	})
	return result
}
func splitFormatted(value formattedText, limit int) []formattedText {
	var chunks []formattedText
	var b strings.Builder
	units, start := 0, 0
	flush := func() {
		if b.Len() == 0 {
			return
		}
		end := start + units
		chunk := formattedText{Text: b.String()}
		for _, e := range value.Entities {
			left, right := max(start, e.Offset), min(end, e.Offset+e.Length)
			if right > left {
				e.Offset = left - start
				e.Length = right - left
				chunk.Entities = append(chunk.Entities, e)
			}
		}
		chunks = append(chunks, chunk)
		start = end
		units = 0
		b.Reset()
	}
	for _, r := range value.Text {
		size := utf16.RuneLen(r)
		if units+size > limit {
			flush()
		}
		b.WriteRune(r)
		units += size
	}
	flush()
	return chunks
}
