package mathml

import (
	_ "embed"
	"fmt"
	"sync"

	"github.com/dop251/goja"
)

// temmlSource is Temml v0.13.4. See internal/temml/LICENSE.
//
//go:embed internal/temml/temml.min.js
var temmlSource string

var temmlProgram = goja.MustCompile("temml.min.js", temmlSource, false)

type temmlRuntime struct {
	vm     *goja.Runtime
	render goja.Callable
}

var runtimePool = sync.Pool{New: func() any {
	vm := goja.New()
	if _, err := vm.RunProgram(temmlProgram); err != nil {
		panic(fmt.Sprintf("mathml: initialize Temml: %v", err))
	}
	temml := vm.Get("temml").ToObject(vm)
	render, ok := goja.AssertFunction(temml.Get("renderToString"))
	if !ok {
		panic("mathml: Temml renderToString is not callable")
	}
	return &temmlRuntime{vm: vm, render: render}
}}

const maxExpressionBytes = 1 << 20

func renderMathML(expression []byte, display bool) (string, error) {
	if len(expression) > maxExpressionBytes {
		return "", &RenderError{
			Expression: string(expression),
			Display:    display,
			Err:        fmt.Errorf("expression is %d bytes; maximum is %d", len(expression), maxExpressionBytes),
		}
	}
	runtime := runtimePool.Get().(*temmlRuntime)
	options := runtime.vm.ToValue(map[string]any{
		"annotate":     true,
		"displayMode":  display,
		"strict":       false,
		"throwOnError": true,
		"trust":        false,
		"xml":          true,
	})
	value, err := runtime.render(goja.Undefined(), runtime.vm.ToValue(string(expression)), options)
	if err != nil {
		// Discard a runtime after an exception instead of assuming its state is
		// still suitable for another expression.
		return "", &RenderError{
			Expression: string(expression),
			Display:    display,
			Err:        err,
		}
	}
	runtimePool.Put(runtime)
	return value.String(), nil
}
