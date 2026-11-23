package mathml

import (
	"html"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

type mathRenderer struct {
	fallback bool
	reporter ErrorReporter
}

func (r *mathRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindInline, r.renderInline)
	reg.Register(KindBlock, r.renderBlock)
}

func (r *mathRenderer) renderInline(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	math := node.(*Inline)
	result, err := renderMathML(math.Equation, math.Display)
	if err != nil {
		if !r.fallback {
			return ast.WalkStop, err
		}
		r.report(math.Equation, math.Display, err)
		return renderErrorFallback(w, math.Equation, false)
	}
	if _, err := w.WriteString(result); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

func (r *mathRenderer) renderBlock(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	equation := node.(*Block).Equation
	result, err := renderMathML(equation, true)
	if err != nil {
		if !r.fallback {
			return ast.WalkStop, err
		}
		r.report(equation, true, err)
		status, writeErr := renderErrorFallback(w, equation, true)
		if writeErr == nil {
			writeErr = w.WriteByte('\n')
		}
		return status, writeErr
	}
	if _, err := w.WriteString(result); err != nil {
		return ast.WalkStop, err
	}
	if err := w.WriteByte('\n'); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}

func (r *mathRenderer) report(expression []byte, display bool, err error) {
	if r.reporter != nil {
		r.reporter(string(expression), display, err)
	}
}

func renderErrorFallback(w util.BufWriter, expression []byte, display bool) (ast.WalkStatus, error) {
	escaped := html.EscapeString(string(expression))
	var output string
	if display {
		output = `<div class="math-error" role="note"><strong>Math rendering error</strong><pre><code>` + escaped + `</code></pre></div>`
	} else {
		output = `<span class="math-error" role="note"><strong>Math rendering error:</strong> <code>` + escaped + `</code></span>`
	}
	if _, err := w.WriteString(output); err != nil {
		return ast.WalkStop, err
	}
	return ast.WalkSkipChildren, nil
}
