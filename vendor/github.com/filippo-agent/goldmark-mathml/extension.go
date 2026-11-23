// Package mathml provides a Goldmark extension that parses GitHub-style
// mathematical expressions and renders them server-side as native MathML.
package mathml

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

const (
	priorityMathBlock       = 50
	priorityMathInline      = 50
	priorityMathTransformer = 100
	priorityMathRenderer    = 100
)

type ErrorReporter func(expression string, display bool, err error)

// Option configures the extension.
type Option interface {
	apply(*extension)
}

type optionFunc func(*extension)

func (f optionFunc) apply(e *extension) { f(e) }

// WithErrorFallback makes rendering continue when Temml rejects an expression.
// Invalid inline expressions are emitted as visible warning spans and invalid
// display expressions as warning blocks, with the original expression safely
// escaped. If reporter is non-nil, it is called for each rejected expression
// as an out-of-band notification and does not affect the rendered output.
// Without this option, rendering stops and returns the error.
func WithErrorFallback(reporter ErrorReporter) Option {
	return optionFunc(func(e *extension) {
		e.fallback = true
		e.reporter = reporter
	})
}

type extension struct {
	fallback bool
	reporter ErrorReporter
}

// New returns a Goldmark extension for GitHub-style mathematical expressions.
// It supports $...$ and $`...`$ inline math, $$...$$ display math, and fenced
// code blocks whose language is "math". Expressions are rendered to MathML on
// the server; no client-side JavaScript is emitted.
func New(options ...Option) goldmark.Extender {
	e := &extension{}
	for _, option := range options {
		option.apply(e)
	}
	return e
}

func (e *extension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			util.Prioritized(&blockParser{}, priorityMathBlock),
		),
		parser.WithInlineParsers(
			util.Prioritized(&inlineParser{}, priorityMathInline),
		),
		parser.WithASTTransformers(
			util.Prioritized(&mathFenceTransformer{}, priorityMathTransformer),
		),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(
		util.Prioritized(&mathRenderer{
			fallback: e.fallback,
			reporter: e.reporter,
		}, priorityMathRenderer),
	))
}
