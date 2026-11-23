# goldmark-mathml

`goldmark-mathml` is a [Goldmark](https://github.com/yuin/goldmark)
extension that parses GitHub-style mathematical expressions and renders them
server-side as native MathML.

It emits no client-side JavaScript. The TeX-to-MathML conversion is performed by
an embedded copy of [Temml](https://temml.org/) running in the pure-Go
[Goja](https://github.com/dop251/goja) JavaScript runtime.

## Usage

```go
package main

import (
    "os"

    mathml "github.com/filippo-agent/goldmark-mathml"
    "github.com/yuin/goldmark"
    "github.com/yuin/goldmark/extension"
)

func main() {
    markdown := goldmark.New(goldmark.WithExtensions(
        extension.GFM,
        mathml.New(),
    ))
    if err := markdown.Convert([]byte("Euler wrote $e^{i\\pi}+1=0$."), os.Stdout); err != nil {
        panic(err)
    }
}
```

The extension and its renderer are safe for concurrent use. Initialized
rendering runtimes are pooled and reused.

By default, an invalid expression stops rendering and returns a typed
`*mathml.RenderError`, which callers can identify with `errors.As`. This is
useful in linters and build pipelines. Long-lived servers can instead keep the
page available and emit a visible, safely escaped warning where the expression
would have appeared:

```go
mathml.New(mathml.WithErrorFallback(nil))
```

The fallback itself is always rendered as HTML: a warning containing the safely
escaped original expression. The argument is an optional reporting callback;
it does not produce rendered output. Pass `nil` unless the application has a
specific use for out-of-band error reporting.

The checked-in `testdata/github.json` corpus contains GitHub `/markdown` API
results for delimiter, escaping, currency, Markdown-nesting, and fenced-block
edge cases. Tests compare the inline and display expressions recognized by this
extension with those recognized by GitHub.

For consistent native MathML rendering across browsers, serve the bundled
Temml stylesheet and supplemental font:

```go
mux.Handle("GET /mathml/", http.StripPrefix("/mathml/",
    http.FileServerFS(mathml.Assets())))
```

and include it in the page:

```html
<link rel="stylesheet" href="/mathml/temml.css">
```

The stylesheet is optional for basic MathML. It contains browser workarounds
and improves accents, primes, and script glyphs; it does not contain
JavaScript.

## Syntax

The supported syntax follows [GitHub's documented mathematical-expression
format](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/writing-mathematical-expressions):

````markdown
Inline math: $x^2 + y^2$

Inline math containing Markdown-significant characters: $`x_{*}`$

$$
x^2 + y^2 = z^2
$$

```math
x^2 + y^2 = z^2
```
````

A display expression may also place both `$$` delimiters on one otherwise
empty line:

```markdown
$$x^2 + y^2 = z^2$$
```

To write a literal dollar inside an expression, use GitHub's backtick form and
escape the dollar for TeX:

```markdown
$`\sqrt{\$4}`$
```

To write a literal dollar outside math on a line that also contains an
expression, use GitHub's documented HTML escape:

```markdown
To split <span>$</span>100 in half, calculate $100/2$.
```

Ordinary code spans and code fences are never interpreted as math. A display
block cannot interrupt an existing Markdown paragraph; separate it from prose
with a blank line.

## Errors

Invalid TeX causes Goldmark rendering to return an error. This makes malformed
expressions visible in tests and build pipelines instead of silently emitting
broken markup.

## Third-party code

The embedded `internal/temml/temml.min.js`, `assets/temml.css`, and
`assets/Temml.woff2` are from Temml v0.13.4. Its license is in
`internal/temml/LICENSE`.
