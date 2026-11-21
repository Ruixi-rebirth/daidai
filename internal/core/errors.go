package core

// UncertainExecutionError means an execution-capable provider failed after it
// may have started work, so callers must not transparently replay the request.
type UncertainExecutionError struct {
	Err error
}

func (e *UncertainExecutionError) Error() string { return e.Err.Error() }
func (e *UncertainExecutionError) Unwrap() error { return e.Err }
