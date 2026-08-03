package util

import (
	"testing"

	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestRecoverToError_NoPanicLeavesTheErrorAlone(t *testing.T) {
	logger := ulogger.TestLogger{}

	run := func() (err error) {
		defer RecoverToError(logger, &err, nil, "worker %d", 7)()

		return errors.NewProcessingError("the real error")
	}

	err := run()
	require.ErrorContains(t, err, "the real error", "a normal return must not be rewritten")
}

func TestRecoverToError_PanicBecomesAnErrorWithoutLeakingThePanicValue(t *testing.T) {
	logger := ulogger.TestLogger{}

	run := func() (err error) {
		defer RecoverToError(logger, &err, nil, "worker %d", 7)()

		panic("runtime error: invalid memory address or nil pointer dereference")
	}

	err := run()
	require.Error(t, err, "the panic must be converted into an error, not escape the goroutine")
	require.ErrorContains(t, err, "worker 7", "the error must identify the fan-out site")
	require.NotContains(t, err.Error(), "nil pointer dereference", "the panic value belongs in the log only")
}

func TestRecoverToError_OnPanicRunsWithTheSameError(t *testing.T) {
	logger := ulogger.TestLogger{}

	var cleanedUpWith error

	run := func() (err error) {
		defer RecoverToError(logger, &err, func(e error) { cleanedUpWith = e }, "streamer")()

		panic("boom")
	}

	err := run()
	require.Error(t, err)
	require.Equal(t, err, cleanedUpWith, "onPanic must receive the error the caller will see, so it can fail a pipe with it")
}

func TestRecoverToError_OnPanicIsNotRunOnTheHappyPath(t *testing.T) {
	logger := ulogger.TestLogger{}

	called := false

	run := func() (err error) {
		defer RecoverToError(logger, &err, func(error) { called = true }, "streamer")()

		return nil
	}

	require.NoError(t, run())
	require.False(t, called, "onPanic is panic-only cleanup; a clean return must not trigger it")
}

// The reason the helper exists: errgroup does not propagate panics from its
// children, so a bare g.Go takes the process down with it. Pin that contract so
// nobody "simplifies" the guards away on the assumption errgroup handles it.
func TestRecoverToError_ErrgroupChildPanicIsContained(t *testing.T) {
	logger := ulogger.TestLogger{}

	g := &errgroup.Group{}
	g.Go(func() (err error) {
		defer RecoverToError(logger, &err, nil, "child")()

		panic("boom")
	})

	require.Error(t, g.Wait(), "the group must report the child's panic as an error")
}
