package httpimpl

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Echo's middleware.Recover only wraps the request goroutine. Handlers that fan
// out over errgroup must recover per goroutine or a single bad record takes the
// whole asset process down — and both of these handlers take their keys straight
// from the request body, so the record is attacker-chosen.
//
// The panic these reproduce is not hypothetical: a tx whose .tx external blob is
// absent but whose .outputs blob is present reconstructs with nil *bt.Output
// holes, and every go-bt serialization entry point dereferences them
// (repository.GetTransaction -> txMeta.Tx.ExtendedBytes()).
//
// Without the fix these tests do not fail — they abort the whole test binary,
// which is exactly what they are asserting cannot happen in production.

func TestGetTransactions_PanicInErrgroupGoroutineDoesNotCrashProcess(t *testing.T) {
	initPrometheusMetrics()

	httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

	mockRepo.On("GetTransaction", mock.Anything).Run(func(mock.Arguments) {
		panic("runtime error: invalid memory address or nil pointer dereference")
	}).Return(nil, nil)

	echoContext.Request().Body = io.NopCloser(bytes.NewReader(testTX1Hash.CloneBytes()))

	err := httpServer.GetTransactions()(echoContext)

	require.Error(t, err, "handler must surface an error, not let the panic escape the goroutine")

	httpErr := &echo.HTTPError{}
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusInternalServerError, httpErr.Code)
}

func TestGetUTXOsByTXID_PanicInErrgroupGoroutineDoesNotCrashProcess(t *testing.T) {
	initPrometheusMetrics()

	httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

	mockRepo.On("GetTransaction", mock.Anything).Return(testTX1RawBytes, nil).Once()
	mockRepo.On("GetUtxo", mock.Anything).Run(func(mock.Arguments) {
		panic("runtime error: invalid memory address or nil pointer dereference")
	}).Return(nil, nil)

	echoContext.SetPath("/utxos/txid/:hash")
	echoContext.SetParamNames("hash")
	echoContext.SetParamValues(testTX1Hash.String())

	err := httpServer.GetUTXOsByTxID(JSON)(echoContext)

	require.Error(t, err, "handler must surface an error, not let the panic escape the goroutine")

	httpErr := &echo.HTTPError{}
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusInternalServerError, httpErr.Code)
}
