package mathml

import (
	"bytes"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

type inlineParser struct{}

func (*inlineParser) Trigger() []byte { return []byte{'$'} }

func (*inlineParser) Parse(parent ast.Node, reader text.Reader, _ parser.Context) ast.Node {
	// GitHub leaves dollar syntax literal inside links and emphasis. Goldmark's
	// inline parser sees these delimiters before it has built their AST nodes,
	// so check both the parent and the immediately preceding delimiter.
	switch parent.(type) {
	case *ast.Link, *ast.Emphasis:
		return nil
	}
	preceding := reader.PrecendingCharacter()
	if preceding == '[' || preceding == '*' || preceding == '_' {
		return nil
	}

	line, _ := reader.PeekLine()
	if len(line) < 3 || line[0] != '$' || reader.PrecendingCharacter() == '$' {
		return nil
	}

	// GitHub documents <span>$</span> for a literal dollar on a line that
	// also contains math. Do not let that dollar open an expression.
	if len(line) >= 1+len("</span>") && bytes.EqualFold(line[1:1+len("</span>")], []byte("</span>")) {
		return nil
	}

	if line[1] == '`' {
		return parseBacktickInline(line, reader)
	}

	delimiter := 1
	display := false
	if line[1] == '$' {
		delimiter = 2
		display = true
		if len(line) < 5 { // $$x$$ is the shortest non-empty expression.
			return nil
		}
	}

	body := line[delimiter:]
	if startsWithSpace(body) {
		return nil
	}

	closeAt := findDollarDelimiter(body, delimiter)
	if closeAt < 0 || closeAt == 0 || followedByAlphaNumeric(body, closeAt+delimiter) {
		return nil
	}

	equation := bytes.Clone(body[:closeAt])
	reader.Advance(delimiter + closeAt + delimiter)
	return &Inline{Equation: equation, Display: display}
}

// parseBacktickInline parses GitHub's $`...`$ form. Matching runs of
// backticks are accepted so expressions can themselves contain backticks.
func parseBacktickInline(line []byte, reader text.Reader) ast.Node {
	ticks := 0
	for 1+ticks < len(line) && line[1+ticks] == '`' {
		ticks++
	}
	if ticks == 0 {
		return nil
	}
	closer := append(bytes.Repeat([]byte{'`'}, ticks), '$')
	body := line[1+ticks:]
	closeAt := bytes.Index(body, closer)
	if closeAt < 0 || closeAt == 0 {
		return nil
	}
	equation := bytes.Clone(body[:closeAt])
	reader.Advance(1 + ticks + closeAt + len(closer))
	return &Inline{Equation: equation}
}

func findDollarDelimiter(body []byte, count int) int {
	for i := 0; i+count <= len(body); i++ {
		if body[i] != '$' || (count == 2 && body[i+1] != '$') {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && body[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			// GitHub's documented way to include an escaped dollar in math is
			// the $`...`$ form. Do not reinterpret a later dollar as the close
			// delimiter of a regular $...$ expression.
			return -1
		}
		return i
	}
	return -1
}

func startsWithSpace(b []byte) bool {
	r, _ := utf8.DecodeRune(b)
	return unicode.IsSpace(r)
}

func followedByAlphaNumeric(body []byte, after int) bool {
	if after >= len(body) {
		return false
	}
	r, _ := utf8.DecodeRune(body[after:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
