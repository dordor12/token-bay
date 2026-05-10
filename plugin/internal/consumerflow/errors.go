package consumerflow

import "errors"

// ErrInvalidDeps is the chain root for Deps validation failures. Callers
// classify with errors.Is.
var ErrInvalidDeps = errors.New("consumerflow: invalid Deps")
