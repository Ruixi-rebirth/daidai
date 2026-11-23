package mathml

import (
	"bytes"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// mathFenceTransformer converts GitHub's fenced ```math blocks after the
// standard fenced-code parser has protected their contents from Markdown.
type mathFenceTransformer struct{}

func (*mathFenceTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var fences []*ast.FencedCodeBlock
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if fence, ok := node.(*ast.FencedCodeBlock); ok &&
				bytes.Equal(fence.Language(source), []byte("math")) {
				fences = append(fences, fence)
			}
		}
		return ast.WalkContinue, nil
	})
	for _, fence := range fences {
		parent := fence.Parent()
		if parent == nil {
			continue
		}
		equation := bytes.TrimSuffix(fence.Lines().Value(source), []byte("\n"))
		if len(equation) == 0 {
			continue
		}
		parent.ReplaceChild(parent, fence, &Block{Equation: bytes.Clone(equation)})
	}
}
