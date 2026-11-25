package render

import (
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type starDelimiterProcessor struct{}

func (p *starDelimiterProcessor) IsDelimiter(b byte) bool {
	return b == '*'
}

func (p *starDelimiterProcessor) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}

func (p *starDelimiterProcessor) OnMatch(consumes int) ast.Node {
	return ast.NewEmphasis(consumes)
}

var cjkStarProcessor = &starDelimiterProcessor{}

// cjkEmphasisParser relaxes CommonMark's punctuation-adjacency rule for '*'
// when bordering CJK characters or CJK punctuation, where spaces are omitted
// around inline code, quotes, brackets, or math expressions.
type cjkEmphasisParser struct{}

func newCJKEmphasisParser() parser.InlineParser {
	return &cjkEmphasisParser{}
}

func (p *cjkEmphasisParser) Trigger() []byte {
	return []byte{'*'}
}

func (p *cjkEmphasisParser) Parse(_ ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	length := 0
	for length < len(line) && line[length] == '*' {
		length++
	}
	if length == 0 {
		return nil
	}

	after := rune(' ')
	if length < len(line) {
		after = util.ToRune(line, length)
	}

	beforeIsSpace := util.IsSpaceRune(before)
	afterIsSpace := util.IsSpaceRune(after)
	beforeIsPunct := util.IsPunctRune(before)
	afterIsPunct := util.IsPunctRune(after)

	isLeft := !afterIsSpace && (!afterIsPunct || beforeIsSpace || beforeIsPunct || isCJKIdeographOrKana(before) || isCJKPunct(after))
	isRight := !beforeIsSpace && (!beforeIsPunct || afterIsSpace || afterIsPunct || isCJKIdeographOrKana(after) || isCJKPunct(before))

	node := parser.NewDelimiter(isLeft, isRight, length, '*', cjkStarProcessor)
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(node.OriginalLength)
	pc.PushDelimiter(node)
	return node
}

func isCJKIdeographOrKana(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isCJKPunct(r rune) bool {
	if (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF01 && r <= 0xFF60) || (r >= 0xFFE0 && r <= 0xFFE6) {
		return true
	}
	switch r {
	case '‘', '’', '“', '”', '—', '…', '·':
		return true
	}
	return false
}

// withCJKEmphasis returns a parser option enabling CJK-friendly '*' emphasis
// while leaving '_' underscore handling on Goldmark's default parser.
func withCJKEmphasis() goldmark.Option {
	return goldmark.WithParserOptions(
		parser.WithInlineParsers(
			util.Prioritized(newCJKEmphasisParser(), 499),
		),
	)
}
