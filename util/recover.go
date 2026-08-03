package util

import (
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/ulogger"
)

// RecoverToError converts a panic in the calling goroutine into an error stored
// in *retErr, for use as the first line of a fan-out goroutine.
//
// errgroup deliberately does not propagate panics from its children
// (golang.org/x/sync/errgroup), and echo's middleware.Recover only wraps the
// request goroutine, so a bare g.Go on a request path takes the whole process
// down with it. Goroutines that outlive their request — the ones streaming into
// an io.Pipe whose reader is handed back to the caller — cannot be reached by
// middleware.Recover even in principle.
//
// The panic value goes to the log; the returned error names only the fan-out
// site, so no panic detail reaches a client.
//
//	g.Go(func() (err error) {
//		defer util.RecoverToError(logger, &err, nil, "getTxs batch at %d", offset)()
//		...
//	})
//
// onPanic, when non-nil, runs after *retErr is set and receives that same error.
// Use it for the cleanup a panic would otherwise skip — failing an io.Pipe the
// consumer is blocked on, or aborting a half-written blob. It does not run on a
// normal return, so a goroutine's own error paths keep their existing handling.
//
// Parameters:
//   - logger: receives the panic value and the fan-out site label
//   - retErr: named return of the fan-out goroutine, set only on panic
//   - onPanic: optional panic-only cleanup, receives the error stored in retErr
//   - format, args: identifies the fan-out site in both the log and the error
func RecoverToError(logger ulogger.Logger, retErr *error, onPanic func(err error), format string, args ...any) func() {
	return func() {
		r := recover()
		if r == nil {
			return
		}

		logger.Errorf("recovered panic in "+format+": %v", append(args, r)...)

		err := errors.NewProcessingError("internal error in "+format, args...)
		*retErr = err

		if onPanic != nil {
			onPanic(err)
		}
	}
}
