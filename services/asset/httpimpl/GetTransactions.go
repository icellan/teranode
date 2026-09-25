package httpimpl

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/labstack/echo/v4"
	"golang.org/x/sync/errgroup"
)

// GetTransactions creates an HTTP handler for retrieving multiple transactions in a single request.
// It accepts a stream of transaction hashes and returns concatenated transaction data.
//
// HTTP Method:
//   - POST
//
// Request:
//
//	Content-Type: application/octet-stream
//	Body: Concatenated 32-byte transaction hashes
//	Note: Each hash must be exactly 32 bytes
//
// Returns:
//   - func(c echo.Context) error: Echo handler function
//
// HTTP Response:
//
//	Status: 200 OK
//	Content-Type: application/octet-stream
//	Body: Concatenated transaction data for all found transactions
//
// Error Responses:
//
//   - 404 Not Found:
//
//   - One or more transactions not found
//     Example: {"message": "not found"}
//
//   - 500 Internal Server Error:
//
//   - Error reading request body
//
//   - Invalid hash format
//
//   - Repository errors
//
// Performance:
//   - Concurrent transaction retrieval (up to 1024 goroutines)
//   - The response buffer is sized from the serialized transactions, not from
//     the requested record count
//   - Each hash is dispatched to a lookup as it is read from the request body,
//     rather than reading the whole body before any lookup starts
//   - Results are written into per-record slots (a mutex-guarded slice that
//     grows one slot per hash read, not presized from a request-supplied count)
//
// Monitoring:
//   - Execution time recorded in "GetTransactions_http" statistic
//   - Prometheus metric "asset_http_get_transactions" tracks number of transactions
//   - Debug logging includes:
//   - Number of transactions processed
//   - Total response size
//   - Processing duration
//
// Example Usage:
//
//	# Request multiple transactions
//	POST /transactions
//	Body: <32-byte-hash1><32-byte-hash2>...
//
// Notes:
//   - Each transaction hash in the request must be exactly 32 bytes
//   - Response contains only found transactions
//   - Transactions are retrieved concurrently for better performance
//   - Response order matches request order
func (h *HTTP) GetTransactions() func(c echo.Context) error {
	return func(c echo.Context) error {
		ctx, _, deferFn := tracing.Tracer("asset").Start(c.Request().Context(), "GetTransactions_http",
			tracing.WithParentStat(AssetStat),
			tracing.WithDebugLogMessage(h.logger, "[Asset_http:GetTransactions] for %s", c.RealIP()),
		)

		defer deferFn()

		var subtreeHash *chainhash.Hash

		// this if statement is temporary and will be removed when the subtree data is implemented
		if len(c.Param("hash")) > 0 {
			if len(c.Param("hash")) != 64 {
				return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid subtree hash length").Error())
			}

			hash, err := chainhash.NewHashFromStr(c.Param("hash"))
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid subtree hash string", err).Error())
			}

			// check whether the subtree exists
			subtreeExists, err := h.repository.GetSubtreeExists(ctx, hash)
			if err != nil {
				if errors.Is(err, errors.ErrNotFound) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such file") {
					return echo.NewHTTPError(http.StatusNotFound, errors.NewNotFoundError("subtree %s not found", hash.String(), err).Error())
				} else {
					return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error getting subtree", hash.String(), err).Error())
				}
			}

			if !subtreeExists {
				return echo.NewHTTPError(http.StatusNotFound, errors.NewNotFoundError("subtree %s not found", hash.String()).Error())
			}

			subtreeHash = hash
		}

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEOctetStream)

		body := c.Request().Body
		defer func() {
			_ = body.Close()
		}()

		// Read the first hash before deciding whether to load the subtree
		// transaction map: an empty body must not pay for a full subtree-data
		// deserialization the request never needed.
		var hash chainhash.Hash

		if _, err := io.ReadFull(body, hash[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return c.Blob(http.StatusOK, echo.MIMEOctetStream, nil)
			}

			return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error reading request body", err).Error())
		}

		transactionFromSubtreeData := make(map[chainhash.Hash]*bt.Tx)

		// The permit covers the map, so it is held for as long as the map is read -
		// which ends with the fan-out below, not with the response. Holding it across
		// the write would pace a node-wide semaphore off the client's read speed.
		releaseSubtreeMap := func() {}
		defer func() { releaseSubtreeMap() }()

		if subtreeHash != nil {
			// read the data from the subtreeData file and create a map of transaction hashes to transactions
			txMap, release, err := h.repository.GetSubtreeTransactions(ctx, subtreeHash)

			releaseSubtreeMap = release

			if err != nil {
				// this should not be an ERROR, but a warning, because it is not critical if the subtree data is not available
				// it just means that the transactions will be fetched from the utxo store
				h.logger.Debugf("[Asset_http:GetTransactions][%s] error getting transactions from subtree data: %s", subtreeHash.String(), err.Error())
			} else {
				transactionFromSubtreeData = txMap
			}
		}

		maxRecords := h.maxBatchRecords()

		var (
			partsMu      sync.Mutex
			parts        [][]byte
			responseSize atomic.Int64
			recordsRead  int
		)

		g, gCtx := errgroup.WithContext(ctx)
		util.SafeSetLimit(h.logger, g, 1024)

		// dispatch budgets and launches the lookup for a single hash, appending it
		// its own slot in parts. parts grows one slot per hash actually read, so
		// it is never presized from a request-supplied count; the mutex guards
		// both the append and every goroutine's indexed write, since a later
		// append can reallocate the backing array while an earlier goroutine is
		// still writing to its slot.
		dispatch := func(hash chainhash.Hash) error {
			if maxRecords > 0 && recordsRead >= maxRecords {
				return errBatchRecords(maxRecords)
			}

			recordsRead++

			partsMu.Lock()
			idx := len(parts)
			parts = append(parts, nil)
			partsMu.Unlock()

			g.Go(func() (retErr error) {
				// Echo's middleware.Recover only protects the request goroutine —
				// not the ones errgroup spawns. The hashes come straight from the
				// request body, so without this defer a single caller-chosen txid
				// that panics on serialization (e.g. a tx reconstructed from an
				// .outputs-only external blob, which carries nil *bt.Output holes)
				// would crash the asset process.
				defer func() {
					if r := recover(); r != nil {
						h.logger.Errorf("[Asset_http:GetTransactions] recovered panic on %s: %v", hash.String(), r)
						retErr = echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("internal error getting transaction %s", hash.String()).Error())
					}
				}()

				// A sibling lookup has already failed, or the client went away:
				// skip the work rather than serializing a transaction nobody will
				// read.
				if gCtx.Err() != nil {
					return gCtx.Err()
				}

				var b []byte

				if tx, ok := transactionFromSubtreeData[hash]; ok {
					// always write the non-extended normal bytes as a response !
					// our peer node should extend the transactions if needed
					b = tx.Bytes()
				} else {
					var err error

					b, err = h.repository.GetTransaction(gCtx, &hash)
					if err != nil {
						if errors.Is(err, errors.ErrNotFound) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such file") {
							return echo.NewHTTPError(http.StatusNotFound, errors.NewNotFoundError("transaction not found", err).Error())
						} else {
							return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error getting transaction", err).Error())
						}
					}
				}

				// Reserve the response-byte budget before storing the result: a
				// batch that trips the budget on this record must not retain its
				// bytes in parts.
				if err := h.enforceBatchResponseBytes("GetTransactions", responseSize.Add(int64(len(b))), h.maxBatchResponseBytes()); err != nil {
					return err
				}

				partsMu.Lock()
				parts[idx] = b
				partsMu.Unlock()

				return nil
			})

			return nil
		}

		// firstErr is the read/budget error that stopped the read loop, if any.
		// It takes priority over waitErr: it is the reason no more hashes were
		// read, whereas waitErr is whatever a dispatched lookup returned.
		var firstErr error

		if err := dispatch(hash); err != nil {
			firstErr = err
		}

		for firstErr == nil {
			if gCtx.Err() != nil {
				// A dispatched lookup already failed, or the client's context was
				// cancelled: stop reading more hashes from a body that can no
				// longer produce a successful response.
				break
			}

			if _, err := io.ReadFull(body, hash[:]); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				firstErr = echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error reading request body", err).Error())

				break
			}

			if err := dispatch(hash); err != nil {
				firstErr = err
				break
			}
		}

		h.observeBatchRecords("GetTransactions", recordsRead)

		waitErr := g.Wait()

		// The map is no longer read past this point, so drop the permit before the
		// client-paced response write. release is sync.Once-guarded, so the deferred
		// call above stays safe.
		releaseSubtreeMap()

		if firstErr != nil {
			return firstErr
		}

		if waitErr != nil {
			h.logger.Errorf("failed to get txs from repository: %s", waitErr.Error())
			return waitErr
		}

		responseBytes := concatTransactionBytes(parts)

		prometheusAssetHTTPGetTransactions.WithLabelValues("OK", "200").Add(float64(recordsRead))

		h.logger.Debugf("[Asset_http:GetTransactions] sending %d txs to client (%d bytes)", recordsRead, len(responseBytes))

		return c.Blob(http.StatusOK, echo.MIMEOctetStream, responseBytes)
	}
}

// concatTransactionBytes joins the per-record serialized transactions into a
// single buffer whose capacity is exactly the serialized length. The handler
// used to reserve a flat 32MB before reading the first body byte, so every
// in-flight request on this unauthenticated route cost 32MB regardless of how
// much data it actually asked for. Callers build parts by growing it one slot
// per hash read from the request body, not by presizing it from a
// request-supplied record count, so this function never sees more capacity
// than records actually dispatched.
func concatTransactionBytes(parts [][]byte) []byte {
	total := 0

	for _, p := range parts {
		total += len(p)
	}

	buf := make([]byte, 0, total)

	for _, p := range parts {
		buf = append(buf, p...)
	}

	return buf
}

// batchBudgetWarnInterval rate-limits the "would have been rejected" warnings so
// a flood of oversized batches cannot itself become a logging DoS.
const batchBudgetWarnInterval = time.Minute

// batchResponseWarnBytes is the observation threshold for the response-byte
// budget while asset_maxBatchResponseBytes is unset. 32MiB is the size the old
// unconditional preallocation implicitly treated as a normal batch response.
const batchResponseWarnBytes = 32 * 1024 * 1024

// batchBudgetWarner emits at most one warning per batchBudgetWarnInterval.
type batchBudgetWarner struct {
	lastUnixNano atomic.Int64
}

func (w *batchBudgetWarner) allow() bool {
	now := time.Now().UnixNano()
	last := w.lastUnixNano.Load()

	if now-last < int64(batchBudgetWarnInterval) {
		return false
	}

	return w.lastUnixNano.CompareAndSwap(last, now)
}

//nolint:gochecknoglobals // rate-limit state for the budget warnings, shared by the batch handlers
var (
	batchRecordWarner   batchBudgetWarner
	batchResponseWarner batchBudgetWarner
)

// maxBatchRecords returns the record cap enforced on POST /subtree/:hash/txs.
//
// asset_maxBatchRecords is the operator knob, but this route is the peer-catchup
// path: subtree validation posts subtreevalidation_missingTransactionsBatchSize
// txids (16384 by default) in one request, and a rejection there is not retried
// — it feeds publishInvalidSubtree and demotes an honest peer. The cap is
// therefore never enforced below the configured client batch size, so a
// mis-sized asset_maxBatchRecords cannot silently break synchronisation.
func (h *HTTP) maxBatchRecords() int {
	configured := h.settings.Asset.MaxBatchRecords
	if configured <= 0 {
		return 0
	}

	if floor := h.settings.SubtreeValidation.MissingTransactionsBatchSize; configured < floor {
		return floor
	}

	return configured
}

// errBatchRecords builds the rejection returned once the record budget is set
// and exceeded.
func errBatchRecords(maxRecords int) error {
	return echo.NewHTTPError(http.StatusRequestEntityTooLarge, errors.NewInvalidArgumentError("batch exceeds asset_maxBatchRecords (%d)", maxRecords).Error())
}

// observeBatchRecords logs a rate-limited warning when a batch would have
// breached a record budget the operator has not set yet, so the setting can be
// sized from real traffic before it is turned on.
func (h *HTTP) observeBatchRecords(route string, records int) {
	if h.settings.Asset.MaxBatchRecords > 0 {
		return
	}

	threshold := h.settings.SubtreeValidation.MissingTransactionsBatchSize
	if threshold <= 0 || records <= threshold {
		return
	}

	if batchRecordWarner.allow() {
		h.logger.Warnf("[Asset_http:%s] batch of %d records exceeds the catchup-sized threshold %d, asset_maxBatchRecords is unset (unlimited)", route, records, threshold)
	}
}

// catchupResponseBytesPerTx is the conservative per-transaction size used to floor
// asset_maxBatchResponseBytes at a legitimate maximal catchup batch. It matches the
// ratio batchResponseWarnBytes already assumes for the default batch size (32MiB /
// 16384 = 2KiB/tx).
const catchupResponseBytesPerTx = 2048

// maxBatchResponseBytes returns the response-byte cap enforced on POST
// /subtree/:hash/txs, floored the same way maxBatchRecords is.
//
// asset_maxBatchResponseBytes is the operator knob, but /subtree/:hash/txs is the
// peer-catchup path: subtree validation posts subtreevalidation_missingTransactionsBatchSize
// txids (16384 by default) in one request, each resolving to a full transaction. A
// 413 there is not retried — it feeds publishInvalidSubtree and demotes an honest
// peer. The cap is therefore never enforced below the response size a maximal
// catchup batch can reasonably produce.
//
// POST /utxos is not on the peer-catchup path (see GetUTXOs.go), so it enforces
// the operator's configured value directly, unfloored, the same as its record
// budget.
func (h *HTTP) maxBatchResponseBytes() int64 {
	configured := h.settings.Asset.MaxBatchResponseBytes
	if configured <= 0 {
		return 0
	}

	if floor := int64(h.settings.SubtreeValidation.MissingTransactionsBatchSize) * catchupResponseBytesPerTx; configured < floor {
		return floor
	}

	return configured
}

// enforceBatchResponseBytes rejects a batch whose accumulated response exceeds
// maxBytes, and otherwise records a rate-limited observation against the
// unfloored asset_maxBatchResponseBytes setting. A record budget alone does not
// bound output: duplicate txids in one batch each expand into a full transaction.
func (h *HTTP) enforceBatchResponseBytes(route string, total, maxBytes int64) error {
	if maxBytes > 0 {
		if total > maxBytes {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, errors.NewInvalidArgumentError("batch response exceeds asset_maxBatchResponseBytes (%d)", maxBytes).Error())
		}

		return nil
	}

	if total > batchResponseWarnBytes && batchResponseWarner.allow() {
		h.logger.Warnf("[Asset_http:%s] batch response of %d bytes exceeds %d, asset_maxBatchResponseBytes is unset (unlimited)", route, total, batchResponseWarnBytes)
	}

	return nil
}
