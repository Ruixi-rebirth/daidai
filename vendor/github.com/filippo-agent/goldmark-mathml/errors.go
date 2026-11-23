package mathml

import "fmt"

// RenderError reports an expression that Temml could not render.
// It can be identified with errors.As.
type RenderError struct {
	Expression string
	Display    bool
	Err        error
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("render %q: %v", e.Expression, e.Err)
}

func (e *RenderError) Unwrap() error { return e.Err }
