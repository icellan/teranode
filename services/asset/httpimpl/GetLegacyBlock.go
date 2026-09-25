package httpimpl

import (
	"net"
	"net/http"
	"strings"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/services/asset/repository"
	"github.com/bsv-blockchain/teranode/util/tracing"
	"github.com/labstack/echo/v4"
)

// GetLegacyBlock creates an HTTP handler that streams a block in the legacy Bitcoin protocol format.
//
// Parameters:
//   - c: Echo context containing the HTTP request and response
//
// URL Parameters:
//   - hash: Block hash (hex string)
//
// Returns:
//   - error: Any error encountered during processing
//
// HTTP Response:
//
//	Status: 200 OK
//	Content-Type: application/octet-stream
//	Body: Legacy format block data:
//	  - Magic number (4 bytes): 0xf9, 0xbe, 0xb4, 0xd9
//	  - Block size (4 bytes): little-endian uint32
//	  - Block header (80 bytes)
//	  - Transaction count (VarInt)
//	  - Transactions (variable length):
//	    * Coinbase transaction first
//	    * Followed by all other transactions if present
//
// Error Responses:
//   - 404 Not Found: Block not found
//   - 500 Internal Server Error:
//   - Invalid block hash format
//   - Block retrieval errors
//
// Monitoring:
//   - Prometheus metric "asset_http_get_block_legacy" tracks responses with status
//
// Example Usage:
//
//	GET /block/legacy/<hash>
func (h *HTTP) GetLegacyBlock() func(c echo.Context) error {
	return func(c echo.Context) error {
		// Dispatch to mining candidate handler when ?type=miningcandidate
		if c.QueryParam("type") == "miningcandidate" {
			return h.handleMiningCandidateLegacyBlock(c)
		}

		hashStr := c.Param("hash")

		ctx, _, deferFn := tracing.Tracer("asset").Start(c.Request().Context(), "GetLegacyBlock_http",
			tracing.WithParentStat(AssetStat),
			tracing.WithDebugLogMessage(h.logger, "[Asset_http] GetLegacyBlock for %s: %s", c.RealIP(), hashStr),
		)

		defer deferFn()

		if len(hashStr) != 64 {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid block hash length").Error())
		}

		hash, err := chainhash.NewHashFromStr(hashStr)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid block hash format", err).Error())
		}

		wireBlock := c.QueryParam("wire") != ""

		// The internal legacy-peer-server pool is only for this node's own legacy
		// peer server (pushBlockMsg), which reaches this route over loopback. wire=1
		// alone proves nothing: any anonymous caller can set it. isLoopbackDirectPeer
		// checks the request's actual TCP peer (RemoteAddr), never a header, so a
		// spoofed X-Forwarded-For cannot claim the internal pool from off-box.
		if wireBlock && isLoopbackDirectPeer(c) {
			ctx = repository.WithLegacyBlockReaderPeerPool(ctx, true)
		}

		r, err := h.repository.GetLegacyBlockReader(ctx, hash, wireBlock)
		if err != nil {
			if errors.Is(err, errors.ErrNotFound) || strings.Contains(err.Error(), "not found") {
				prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusNotFound)).Inc()
				return echo.NewHTTPError(http.StatusNotFound, err.Error())
			} else {
				prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusInternalServerError)).Inc()
				return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
			}
		}

		// The reader is a *io.PipeReader fed by a goroutine. Closing it unblocks that
		// goroutine with io.ErrClosedPipe, which it already handles. Without this, a
		// stream that fails because the write to the response failed (client gone) leaves
		// the producer blocked in Write forever, holding its arena and any file-store read
		// permit — streamOrAbort hijacks and returns nil in that case, so nothing else
		// ever closes the read end.
		defer r.Close()

		// Outcome counted after streaming — see the note in GetSubtreeData for why, and
		// for which cases this still labels as OK.
		if err := streamOrAbort(c, http.StatusOK, echo.MIMEOctetStream, r); err != nil {
			prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusInternalServerError)).Inc()

			return err
		}

		prometheusAssetHTTPGetBlockLegacy.WithLabelValues("OK", "200").Inc()

		return nil
	}
}

// GetRestLegacyBlock creates an HTTP handler that streams a block in the legacy Bitcoin protocol format,
// using a ".bin" extension in the URL. Functionally identical to GetLegacyBlock but with different URL format.
//
// URL Parameters:
//   - hash: Block hash followed by ".bin" extension
//     Example: "000000...hash.bin"
//
// All other aspects (response format, errors, monitoring) are identical to GetLegacyBlock.
//
// Example Usage:
//
//	GET /block/legacy/<hash>.bin
func (h *HTTP) GetRestLegacyBlock() func(c echo.Context) error {
	return func(c echo.Context) error {
		resource := c.Param("hash.bin")
		if resource == "" {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid block hash extension").Error())
		}

		hashString := strings.Replace(resource, ".bin", "", 1)

		ctx, _, deferFn := tracing.Tracer("asset").Start(c.Request().Context(), "GetRestLegacyBlock_http",
			tracing.WithParentStat(AssetStat),
			tracing.WithDebugLogMessage(h.logger, "[Asset_http] GetBlockGetRestLegacyBlockByHash for %s: %s", c.RealIP(), resource),
		)

		defer deferFn()

		if len(hashString) != 64 {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid block hash length").Error())
		}

		hash, err := chainhash.NewHashFromStr(hashString)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, errors.NewInvalidArgumentError("invalid block hash string", err).Error())
		}

		r, err := h.repository.GetLegacyBlockReader(ctx, hash)
		if err != nil {
			if errors.Is(err, errors.ErrNotFound) || strings.Contains(err.Error(), "not found") {
				prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusNotFound)).Inc()

				return echo.NewHTTPError(http.StatusNotFound, errors.NewNotFoundError("block not found", err).Error())
			} else {
				prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusInternalServerError)).Inc()

				return echo.NewHTTPError(http.StatusInternalServerError, errors.NewProcessingError("error getting block", err).Error())
			}
		}

		// See GetLegacyBlock: closing the pipe read end is what unblocks the producer
		// goroutine when the response write failed and streamOrAbort hijacked.
		defer r.Close()

		// Outcome counted after streaming — see the note in GetSubtreeData.
		if err := streamOrAbort(c, http.StatusOK, echo.MIMEOctetStream, r); err != nil {
			prometheusAssetHTTPGetBlockLegacy.WithLabelValues("ERROR", http.StatusText(http.StatusInternalServerError)).Inc()

			return err
		}

		prometheusAssetHTTPGetBlockLegacy.WithLabelValues("OK", "200").Inc()

		return nil
	}
}

// isLoopbackDirectPeer reports whether c's request arrived directly from a
// loopback address, using the connection's actual remote address
// (http.Request.RemoteAddr) rather than c.RealIP() or any client-supplied header
// (X-Forwarded-For, X-Real-IP): those are exactly what an anonymous caller
// controls, and the internal legacy-peer-server pool must not be claimable from
// off-box by setting one.
//
// This holds only when the legacy peer server reaches Asset HTTP over an actual
// loopback connection. Behind a same-pod sidecar proxy (or any proxy terminating
// the TCP connection locally and forwarding on), every request's direct peer is
// the proxy itself — typically also loopback — so the separation does not
// distinguish the legacy peer server from other proxied traffic in that topology.
func isLoopbackDirectPeer(c echo.Context) bool {
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		// RemoteAddr without a port (e.g. a unix socket, or a malformed test
		// request) is never a loopback TCP peer.
		return false
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
