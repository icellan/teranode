package httpimpl

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestGetTransactions(t *testing.T) {
	initPrometheusMetrics()

	t.Run("Valid transaction hashes", func(t *testing.T) {
		httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)

		// Set up mock responses for transaction hashes
		// We use mock.Anything for the hash parameter since the exact value isn't critical for this test
		mockRepo.On("GetTransaction", mock.Anything).Return(testTX1RawBytes, nil).Once()
		mockRepo.On("GetTransaction", mock.Anything).Return(testTX2RawBytes, nil).Once()

		// Create a slice with both transaction hashes
		transactionHashes := append(testTX1Hash.CloneBytes(), testTX2Hash.CloneBytes()...)

		// Set up the request
		echoContext.Request().Body = io.NopCloser(bytes.NewReader(transactionHashes))

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		require.NoError(t, err)

		// Check response status code
		assert.Equal(t, http.StatusOK, responseRecorder.Code)

		// Verify transactions using helper function
		verifyTransactions(t, bytes.NewReader(responseRecorder.Body.Bytes()), testTX1Hash.String(), testTX2Hash.String())
	})

	t.Run("Valid transaction hashes with subtree hash", func(t *testing.T) {
		httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)

		subtreeHash := chainhash.HashH([]byte("subtreeHash"))

		emptyTxMap := make(map[chainhash.Hash]*bt.Tx)

		// set mock response
		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil).Once()
		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX2RawBytes, nil).Once()
		mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(true, nil).Once()
		mockRepo.On("GetSubtreeTransactions", mock.Anything, mock.Anything).Return(emptyTxMap, nil)

		transactionHashes := append(testTX1Hash.CloneBytes(), testTX2Hash.CloneBytes()...)

		// Set up the request with subtree hash
		echoContext.SetPath("/subtree/:hash/txs")
		echoContext.SetParamNames("hash")
		echoContext.SetParamValues(subtreeHash.String())
		echoContext.Request().Body = io.NopCloser(bytes.NewReader(transactionHashes))

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		require.NoError(t, err)

		// Check response status code
		assert.Equal(t, http.StatusOK, responseRecorder.Code)

		// Verify transactions using helper function
		verifyTransactions(t, bytes.NewReader(responseRecorder.Body.Bytes()), testTX1Hash.String(), testTX2Hash.String())
	})

	t.Run("With invalid subtree hash", func(t *testing.T) {
		httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

		subtreeHash := chainhash.HashH([]byte("subtreeHash"))

		// set mock response
		mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(false, nil).Once()
		mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(true, nil)

		// set echo context
		echoContext.Request().Header.Set(echo.HeaderContentType, echo.MIMEOctetStream)
		echoContext.SetPath("/:hash/txs")
		echoContext.SetParamNames("hash")
		echoContext.SetParamValues(subtreeHash.String())

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NOT_FOUND")

		echoContext.SetParamValues("test")

		// Call GetTransactions handler
		err = httpServer.GetTransactions()(echoContext)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid subtree hash length")

		echoContext.SetParamValues("testtesttesttesttesttesttesttesttesttesttesttesttesttesttesttest")

		// Call GetTransactions handler
		err = httpServer.GetTransactions()(echoContext)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid subtree hash string")
	})

	t.Run("Invalid transaction hash length", func(t *testing.T) {
		httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil)

		// set echo context
		echoContext.Request().Header.Set(echo.HeaderContentType, echo.MIMEOctetStream)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader([]byte{0x01, 0x02, 0x03, 0x04}))

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		echoErr := &echo.HTTPError{}
		require.True(t, errors.As(err, &echoErr))

		// Check response status code
		assert.Equal(t, http.StatusInternalServerError, echoErr.Code)

		// Check response body
		assert.Equal(t, "PROCESSING (4): error reading request body -> UNKNOWN (0): unexpected EOF", echoErr.Message)
	})

	t.Run("Transaction not found", func(t *testing.T) {
		httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

		// set mock response
		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(nil, errors.NewNotFoundError("transaction not found"))

		// set echo context
		echoContext.Request().Header.Set(echo.HeaderContentType, echo.MIMEOctetStream)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader(testTX1Hash.CloneBytes()))

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		echoErr := &echo.HTTPError{}
		require.True(t, errors.As(err, &echoErr))

		// Check response status code
		assert.Equal(t, http.StatusNotFound, echoErr.Code)

		// Check response body
		assert.Equal(t, "NOT_FOUND (3): transaction not found -> NOT_FOUND (3): transaction not found", echoErr.Message)
	})

	t.Run("Repository error", func(t *testing.T) {
		httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)

		// set mock response
		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(nil, errors.NewStorageError("error getting transaction"))

		// set echo context
		echoContext.Request().Header.Set(echo.HeaderContentType, echo.MIMEOctetStream)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader(testTX1Hash.CloneBytes()))

		// Call GetTransactions handler
		err := httpServer.GetTransactions()(echoContext)
		echoErr := &echo.HTTPError{}
		require.True(t, errors.As(err, &echoErr))

		// Check response status code
		assert.Equal(t, http.StatusInternalServerError, echoErr.Code)

		// Check response body
		assert.Equal(t, "PROCESSING (4): error getting transaction -> STORAGE_ERROR (69): error getting transaction", echoErr.Message)
	})
}

// verifyTransactions verifies that the response contains the expected transactions
func verifyTransactions(t *testing.T, responseBody io.Reader, expectedHashes ...string) {
	t.Helper()

	// Convert expected hashes to a map for easier lookup
	expected := make(map[string]bool)
	for _, h := range expectedHashes {
		expected[h] = true
	}

	// Read transactions from response
	var foundHashes []string

	reader := responseBody

	for {
		tx := &bt.Tx{}

		_, err := tx.ReadFrom(reader)
		if err == io.EOF {
			break
		}

		require.NoError(t, err, "Failed to read transaction from response")

		foundHashes = append(foundHashes, tx.TxIDChainHash().String())
	}

	// Verify we found all expected hashes
	for _, hash := range foundHashes {
		_, exists := expected[hash]
		assert.True(t, exists, "Unexpected transaction hash: %s", hash)
		delete(expected, hash)
	}

	assert.Empty(t, expected, "Did not receive all expected transaction hashes")
}

// TestGetTransactionsCatchupBatch is the regression that matters most on this
// route: POST /subtree/:hash/txs is the peer-catchup path and subtree validation
// posts subtreevalidation_missingTransactionsBatchSize txids (16384 by default)
// in a single request. A rejection here is never retried and demotes the honest
// peer, so a full-size batch must still succeed with the shipped defaults.
func TestGetTransactionsCatchupBatch(t *testing.T) {
	initPrometheusMetrics()

	const catchupBatchSize = 16384

	httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)

	tx, err := bt.NewTxFromBytes(testTX1RawBytes)
	require.NoError(t, err)

	subtreeHash := chainhash.HashH([]byte("catchupSubtree"))
	txMap := map[chainhash.Hash]*bt.Tx{*testTX1Hash: tx}

	mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(true, nil).Once()
	mockRepo.On("GetSubtreeTransactions", mock.Anything, mock.Anything).Return(txMap, nil).Once()

	body := make([]byte, 0, catchupBatchSize*chainhash.HashSize)
	for i := 0; i < catchupBatchSize; i++ {
		body = append(body, testTX1Hash.CloneBytes()...)
	}

	echoContext.SetPath("/subtree/:hash/txs")
	echoContext.SetParamNames("hash")
	echoContext.SetParamValues(subtreeHash.String())
	echoContext.Request().Body = io.NopCloser(bytes.NewReader(body))

	require.NoError(t, httpServer.GetTransactions()(echoContext))
	require.Equal(t, http.StatusOK, responseRecorder.Code)
	require.Len(t, responseRecorder.Body.Bytes(), catchupBatchSize*len(testTX1RawBytes))
}

// TestGetTransactionsEmptyBodySkipsSubtreeLoad covers the empty-body vector: the
// full subtree transaction map used to be materialized from the path hash alone,
// before a single body byte was read, so a caller paid nothing for it.
func TestGetTransactionsEmptyBodySkipsSubtreeLoad(t *testing.T) {
	initPrometheusMetrics()

	httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)

	subtreeHash := chainhash.HashH([]byte("emptyBodySubtree"))

	mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(true, nil).Once()
	mockRepo.On("GetSubtreeTransactions", mock.Anything, mock.Anything).Return(make(map[chainhash.Hash]*bt.Tx), nil)

	echoContext.SetPath("/subtree/:hash/txs")
	echoContext.SetParamNames("hash")
	echoContext.SetParamValues(subtreeHash.String())
	echoContext.Request().Body = io.NopCloser(bytes.NewReader(nil))

	require.NoError(t, httpServer.GetTransactions()(echoContext))
	require.Equal(t, http.StatusOK, responseRecorder.Code)
	require.Empty(t, responseRecorder.Body.Bytes())

	mockRepo.AssertNotCalled(t, "GetSubtreeTransactions", mock.Anything, mock.Anything)
}

// TestGetTransactionsRecordBudget checks asset_maxBatchRecords, which defaults to
// 0 (unlimited, today's behaviour) and rejects only once an operator sets it.
func TestGetTransactionsRecordBudget(t *testing.T) {
	initPrometheusMetrics()

	t.Run("unset accepts any count", func(t *testing.T) {
		httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)

		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader(bytes.Repeat(testTX1Hash.CloneBytes(), 8)))

		require.NoError(t, httpServer.GetTransactions()(echoContext))
		require.Equal(t, http.StatusOK, responseRecorder.Code)
	})

	t.Run("set rejects an over-budget batch", func(t *testing.T) {
		httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)
		httpServer.settings.Asset.MaxBatchRecords = 4

		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader(bytes.Repeat(testTX1Hash.CloneBytes(), 5)))

		err := httpServer.GetTransactions()(echoContext)

		echoErr := &echo.HTTPError{}
		require.True(t, errors.As(err, &echoErr))
		require.Equal(t, http.StatusRequestEntityTooLarge, echoErr.Code)
	})

	t.Run("never enforced below the catchup batch size", func(t *testing.T) {
		httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, nil)
		httpServer.settings.Asset.MaxBatchRecords = 4
		httpServer.settings.SubtreeValidation.MissingTransactionsBatchSize = 16384

		mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil)

		echoContext.Request().Body = io.NopCloser(bytes.NewReader(bytes.Repeat(testTX1Hash.CloneBytes(), 64)))

		require.NoError(t, httpServer.GetTransactions()(echoContext))
		require.Equal(t, http.StatusOK, responseRecorder.Code)
	})
}

// TestGetTransactionsResponseByteBudget checks asset_maxBatchResponseBytes. A
// record cap alone does not bound output: duplicate hashes each expand into a
// full transaction.
func TestGetTransactionsResponseByteBudget(t *testing.T) {
	initPrometheusMetrics()

	httpServer, mockRepo, echoContext, _ := GetMockHTTP(t, nil)
	httpServer.settings.Asset.MaxBatchResponseBytes = int64(len(testTX1RawBytes))

	mockRepo.On("GetTransaction", mock.Anything, mock.Anything).Return(testTX1RawBytes, nil)

	echoContext.Request().Body = io.NopCloser(bytes.NewReader(bytes.Repeat(testTX1Hash.CloneBytes(), 4)))

	err := httpServer.GetTransactions()(echoContext)

	echoErr := &echo.HTTPError{}
	require.True(t, errors.As(err, &echoErr))
	require.Equal(t, http.StatusRequestEntityTooLarge, echoErr.Code)
}

// TestConcatTransactionBytesAllocatesExactly guards against a speculative
// preallocation sized from the requested record count rather than from the
// serialized length. The handler used to reserve a flat 32MB per in-flight
// request before reading the first body byte, on an unauthenticated route.
func TestConcatTransactionBytesAllocatesExactly(t *testing.T) {
	parts := [][]byte{testTX1RawBytes, testTX2RawBytes, testTX1RawBytes}

	concatenated := concatTransactionBytes(parts)

	require.Len(t, concatenated, 2*len(testTX1RawBytes)+len(testTX2RawBytes))
	require.Equal(t, len(concatenated), cap(concatenated),
		"capacity must match the serialized length, not a per-record reservation")
}

// permitWriteProbe records whether the subtree-map permit had already been released
// at the moment the response body started being written.
type permitWriteProbe struct {
	http.ResponseWriter

	released         chan struct{}
	releasedAtWrite  atomic.Bool
	observedAnyWrite atomic.Bool
}

func (p *permitWriteProbe) Write(b []byte) (int, error) {
	p.observedAnyWrite.Store(true)

	select {
	case <-p.released:
		p.releasedAtWrite.Store(true)
	default:
	}

	return p.ResponseWriter.Write(b)
}

// TestGetTransactionsReleasesSubtreePermitBeforeResponseWrite pins the permit to the
// lifetime of the subtree map rather than the lifetime of the response.
//
// asset_concurrency_get_subtree_transactions defaults to 2, while a catching-up peer
// issues subtreevalidation_getMissingTransactions (32) concurrent batch requests. If
// the permit is held across the response write, which is paced by the peer's read
// speed, the node serves two catchup batches at a time and the rest stall on the
// semaphore for the 30s acquire deadline.
func TestGetTransactionsReleasesSubtreePermitBeforeResponseWrite(t *testing.T) {
	initPrometheusMetrics()

	subtreeHash := testSubtree.RootHash()

	body := make([]byte, 0, chainhash.HashSize)
	body = append(body, testTX1Hash.CloneBytes()...)

	httpServer, mockRepo, echoContext, responseRecorder := GetMockHTTP(t, bytes.NewReader(body))

	released := make(chan struct{})
	probe := &permitWriteProbe{ResponseWriter: responseRecorder, released: released}
	echoContext.Response().Writer = probe

	txMap := map[chainhash.Hash]*bt.Tx{*testTX1Hash: testTx1}

	mockRepo.On("GetSubtreeExists", mock.Anything, mock.Anything).Return(true, nil).Once()
	// The repository contract guarantees release is sync.Once-guarded, so mirror
	// that here: the handler legitimately calls it once eagerly and once via defer.
	var releaseOnce sync.Once

	mockRepo.On("GetSubtreeTransactions", mock.Anything, mock.Anything).
		Return(txMap, func() { releaseOnce.Do(func() { close(released) }) }, nil).Once()

	echoContext.SetPath("/subtree/:hash/txs")
	echoContext.SetParamNames("hash")
	echoContext.SetParamValues(subtreeHash.String())

	require.NoError(t, httpServer.GetTransactions()(echoContext))

	require.True(t, probe.observedAnyWrite.Load(), "expected the handler to write a response body")
	require.True(t, probe.releasedAtWrite.Load(),
		"subtree map permit must be released once the fan-out is done, not held across the client-paced response write")
}
