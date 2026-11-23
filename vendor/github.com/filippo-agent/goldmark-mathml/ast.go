package mathml

import (
	"github.com/yuin/goldmark/ast"
)

// Inline is an inline mathematical expression. Display is true for an
// expression delimited by double dollar signs in a paragraph.
type Inline struct {
	ast.BaseInline
	Equation []byte
	Display  bool
}

func (*Inline) Inline()             {}
func (*Inline) IsBlank([]byte) bool { return false }
func (n *Inline) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Equation": string(n.Equation)}, nil)
}
func (*Inline) Kind() ast.NodeKind { return KindInline }

// Block is a display mathematical expression.
type Block struct {
	ast.BaseBlock
	Equation []byte
	closed   bool
}

func (*Block) IsRaw() bool { return true }
func (n *Block) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Equation": string(n.Equation)}, nil)
}
func (*Block) Kind() ast.NodeKind { return KindBlock }

var (
	KindInline = ast.NewNodeKind("MathInline")
	KindBlock  = ast.NewNodeKind("MathBlock")
)
