package mathml

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var displayDelimiter = []byte("$$")

type blockParser struct{}

func (*blockParser) Trigger() []byte { return []byte{'$'} }

func (*blockParser) Open(_ ast.Node, reader text.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	indent := util.TrimLeftSpaceLength(line)
	if indent+1 >= len(line) || line[indent] != '$' || line[indent+1] != '$' {
		return nil, parser.NoChildren
	}

	rest := line[indent+2:]
	node := &Block{}
	if closeAt := bytes.Index(rest, displayDelimiter); closeAt >= 0 {
		// An isolated $$...$$ line is display math. If text follows the
		// closing delimiter, leave it to the inline parser instead.
		if !util.IsBlank(rest[closeAt+2:]) {
			return nil, parser.NoChildren
		}
		node.Equation = bytes.Clone(rest[:closeAt])
		if node.Equation == nil {
			node.Equation = []byte{}
		}
		node.closed = true
		reader.AdvanceToEOL()
		return node, parser.NoChildren
	}

	// Do not claim an unclosed block: GitHub leaves it as literal Markdown.
	// Blank lines end paragraphs and therefore cannot occur inside this form.
	if !hasBlockCloser(reader) {
		return nil, parser.NoChildren
	}

	if !util.IsBlank(rest) {
		node.Lines().Append(text.NewSegment(segment.Start+indent+2, segment.Stop))
	}
	reader.AdvanceToEOL()
	return node, parser.NoChildren
}

func (*blockParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	math := node.(*Block)
	if math.closed {
		return parser.Close
	}

	line, segment := reader.PeekLine()
	closeAt := bytes.Index(line, displayDelimiter)
	if closeAt < 0 {
		math.Lines().Append(segment)
		reader.AdvanceToEOL()
		return parser.Continue | parser.NoChildren
	}

	if !util.IsBlank(line[:closeAt]) {
		math.Lines().Append(text.NewSegment(segment.Start, segment.Start+closeAt))
	}
	reader.Advance(closeAt + 2)
	if util.IsBlank(line[closeAt+2:]) {
		reader.AdvanceToEOL()
	}
	return parser.Close
}

func (*blockParser) Close(node ast.Node, reader text.Reader, _ parser.Context) {
	math := node.(*Block)
	if math.Equation != nil {
		return
	}
	var equation []byte
	for i := 0; i < math.Lines().Len(); i++ {
		segment := math.Lines().At(i)
		equation = append(equation, segment.Value(reader.Source())...)
	}
	math.Equation = equation
	if math.Equation == nil {
		math.Equation = []byte{}
	}
}

func hasBlockCloser(reader text.Reader) bool {
	lineNumber, position := reader.Position()
	defer reader.SetPosition(lineNumber, position)

	reader.AdvanceLine()
	for {
		line, _ := reader.PeekLine()
		if line == nil || util.IsBlank(line) {
			return false
		}
		if closeAt := bytes.Index(line, displayDelimiter); closeAt >= 0 {
			return util.IsBlank(line[closeAt+2:])
		}
		reader.AdvanceLine()
	}
}

// GitHub does not let a display block interrupt an existing paragraph.
func (*blockParser) CanInterruptParagraph() bool { return false }
func (*blockParser) CanAcceptIndentedLine() bool { return false }
