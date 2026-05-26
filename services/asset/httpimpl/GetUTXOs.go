package httpimpl

import (
	"encoding/binary"
	"io"
	"net/http"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/util"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/labstack/echo/v4"
	"golang.org/x/sync/errgroup"
)

const (
	// utxosRequestRecordSize is the on-wire size of one (txid, vout) lookup
	// in the POST /api/v1/utxos request body.
	utxosRequestRecordSize = 36 // 32 bytes txid + 4 bytes vout (little-endian)

	// utxosResponseRecordSize is the on-wire size of one fixed-length response
	// record. Clients index into the response by `i*utxosResponseRecordSize`.
	utxosResponseRecordSize = 48 // 8 bytes status + 4 bytes lockTime + 4 bytes vin + 32 bytes txid

	// utxosFanoutLimit caps in-flight per-record store lookups. Mirrors the
	// limit used by GetTransactions (POST /api/v1/subtree/:hash/txs).
	utxosFanoutLimit = 1024
)

// GetUTXOs creates an HTTP handler for bulk UTXO spend-status lookups.
//
// HTTP Method:
//   - POST
//
// Request:
//
//	Content-Type: application/octet-stream
//	Body: Concatenated 36-byte records, each containing
//	      [32 bytes txid][4 bytes vout (little-endian)].
//	      The body length must be a multiple of 36.
//
// Returns:
//   - func(c echo.Context) error: Echo handler function
//
// HTTP Response:
//
//	Status: 200 OK on any well-formed request, regardless of per-record outcomes.
//	Content-Type: application/octet-stream
//	Body: Concatenated 48-byte records in input order:
//	      [8 bytes status LE][4 bytes lockTime LE][4 bytes vin LE][32 bytes spendingTxID]
//	      For unspent UTXOs the trailing 36 bytes (vin + spendingTxID) are zero-filled.
//	      For records not found in the store, status is utxo.Status_NOT_FOUND and the
//	      remaining bytes are zero. The whole-request HTTP status is unchanged.
//
// Error Responses:
//
//   - 400 Bad Request: body length is not a multiple of 36 bytes.
//   - 413 Request Entity Too Large: body exceeds the asset_httpBodyLimit setting.
//   - 500 Internal Server Error: transport error reading the body, or unrecoverable
//     repository error from at least one record.
//
// Performance notes:
//   - Bypasses the per-record full-transaction fetch that GET /api/v1/utxo/:hash
//     used to do (see GetUTXO.go). Each lookup is a single Aerospike record read.
//   - Concurrent lookups are bounded by utxosFanoutLimit.
//   - Response buffer is preallocated; each goroutine writes its own slot, so no
//     mutex is needed (unlike GetTransactions, where response length is variable).
//
// Monitoring:
//   - Execution time recorded under "GetUTXOs_http" via the asset tracer.
//   - prometheusAssetHTTPGetUTXOs is incremented by the number of records served.
func (h *HTTP) GetUTXOs() func(c echo.Context) error {
	return func(c echo.Context) error {
		ctx, _, deferFn := tracing.Tracer("asset").Start(c.Request().Context(), "GetUTXOs_http",
			tracing.WithParentStat(AssetStat),
			tracing.WithLogMessage(h.logger, "[Asset_http:GetUTXOs] for %s", c.Request().RemoteAddr),
		)

		defer deferFn()

		body := c.Request().Body
		defer func() {
			_ = body.Close()
		}()

		// Read the whole body up front so we can validate its length and index
		// into it without holding the connection open across the fan-out. The
		// asset_httpBodyLimit middleware has already capped this.
		reqBytes, err := io.ReadAll(body)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error reading request body", err).Error())
		}

		if len(reqBytes)%utxosRequestRecordSize != 0 {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("body length %d is not a multiple of %d bytes", len(reqBytes), utxosRequestRecordSize).Error())
		}

		numRecords := len(reqBytes) / utxosRequestRecordSize

		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEOctetStream)

		if numRecords == 0 {
			return c.Blob(http.StatusOK, echo.MIMEOctetStream, nil)
		}

		responseBytes := make([]byte, numRecords*utxosResponseRecordSize)

		g, gCtx := errgroup.WithContext(ctx)
		util.SafeSetLimit(g, utxosFanoutLimit)

		for i := 0; i < numRecords; i++ {
			i := i
			recOffset := i * utxosRequestRecordSize

			var txHash chainhash.Hash
			copy(txHash[:], reqBytes[recOffset:recOffset+32])

			vout := binary.LittleEndian.Uint32(reqBytes[recOffset+32 : recOffset+36])

			g.Go(func() error {
				resp, err := h.repository.GetUtxo(gCtx, &utxo.Spend{
					TxID:         &txHash,
					Vout:         vout,
					UTXOHash:     nil,
					SpendingData: nil,
				})

				slotStart := i * utxosResponseRecordSize
				slot := responseBytes[slotStart : slotStart+utxosResponseRecordSize]

				if err != nil {
					// Per-record not-found becomes Status_NOT_FOUND in the slot;
					// any other store error fails the whole request (we don't
					// want to silently report zero-padded "not found" for every
					// record when the store is unreachable).
					if errors.Is(err, errors.ErrNotFound) {
						writeUTXOsRecord(slot, &utxo.SpendResponse{
							Status: int(utxo.Status_NOT_FOUND),
						})

						return nil
					}

					return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error getting utxo %s:%d", txHash.String(), vout, err).Error())
				}

				if resp == nil {
					resp = &utxo.SpendResponse{Status: int(utxo.Status_NOT_FOUND)}
				}

				writeUTXOsRecord(slot, resp)

				return nil
			})
		}

		if err := g.Wait(); err != nil {
			h.logger.Errorf("[Asset_http:GetUTXOs] fan-out failed: %s", err.Error())

			return err
		}

		prometheusAssetHTTPGetUTXOs.WithLabelValues("OK", "200").Add(float64(numRecords))

		h.logger.Infof("[Asset_http:GetUTXOs] served %d records (%d bytes)", numRecords, len(responseBytes))

		return c.Blob(http.StatusOK, echo.MIMEOctetStream, responseBytes)
	}
}

// writeUTXOsRecord encodes a SpendResponse into dst as a fixed 48-byte record.
// Layout: [8 bytes status LE][4 bytes lockTime LE][4 bytes vin LE][32 bytes spendingTxID].
// dst must have len >= utxosResponseRecordSize. Caller-zeroed slots stay zero
// when SpendingData is nil.
func writeUTXOsRecord(dst []byte, resp *utxo.SpendResponse) {
	binary.LittleEndian.PutUint64(dst[0:8], uint64(resp.Status)) //nolint:gosec
	binary.LittleEndian.PutUint32(dst[8:12], resp.LockTime)

	if resp.SpendingData != nil {
		binary.LittleEndian.PutUint32(dst[12:16], uint32(resp.SpendingData.Vin)) //nolint:gosec

		if resp.SpendingData.TxID != nil {
			copy(dst[16:48], resp.SpendingData.TxID[:])
		}
	}
}
