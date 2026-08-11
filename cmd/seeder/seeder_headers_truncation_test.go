package seeder

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/model"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/services/utxopersister"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/require"
)

// dummyBlockIndexBytes serialises a structurally-valid, but otherwise
// meaningless, BlockIndex record at the given height. Used to build
// synthetic .utxo-headers files without needing a real chain-linked block:
// the truncation tests below only need the *earlier* record(s) to parse
// successfully and the *last* record to be cut short - the content of a
// fully-written record is otherwise irrelevant to what's under test.
func dummyBlockIndexBytes(t *testing.T, height uint32) []byte {
	t.Helper()

	bi := &utxopersister.BlockIndex{
		Hash:    &chainhash.Hash{},
		Height:  height,
		TxCount: 1,
		BlockHeader: &model.BlockHeader{
			HashPrevBlock:  &chainhash.Hash{},
			HashMerkleRoot: &chainhash.Hash{},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, bi.Serialise(&buf))

	return buf.Bytes()
}

// writeUtxoHeadersFile assembles a .utxo-headers file byte for byte in the
// layout WriteHeadersToStore writes (services/utxopersister/UTXOSet.go): file
// header, tip hash (32 bytes), tip height (4 bytes), then the given raw
// per-block record bytes concatenated verbatim - a caller can pass a
// deliberately truncated final record to simulate a cut-off snapshot.
func writeUtxoHeadersFile(t *testing.T, tipHeight uint32, records ...[]byte) string {
	t.Helper()

	header := fileformat.NewHeader(fileformat.FileTypeUtxoHeaders)

	data := make([]byte, 0)
	data = append(data, header.Bytes()...)

	var tipHash chainhash.Hash
	tipHash[0] = 0xee
	data = append(data, tipHash[:]...)

	heightBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(heightBytes, tipHeight)
	data = append(data, heightBytes...)

	for _, r := range records {
		data = append(data, r...)
	}

	path := filepath.Join(t.TempDir(), "truncated.utxo-headers")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

// TestProcessHeaders_TruncatedFile_ReturnsError is the regression test for the
// silent-truncation bug in processHeaders's utxo-headers read loop: cutting a
// file off mid-record - here, 10 bytes into the second block's 32-byte hash
// field - must fail the import outright rather than being misread as a clean
// end-of-records boundary. NewUTXOHeaderFromReader wraps the resulting short
// read (io.ErrUnexpectedEOF) into a *errors.Error whose rendered message
// contains "unexpected EOF"; the old `errors.Is(err, io.EOF)` check matched it
// via this package's substring-matching Is() fallback (io.EOF's message "EOF"
// is a substring of "unexpected EOF"), silently treating the truncated
// snapshot as a complete, successful import of just the genesis header.
func TestProcessHeaders_TruncatedFile_ReturnsError(t *testing.T) {
	genesisRecord := dummyBlockIndexBytes(t, 0)
	secondRecord := dummyBlockIndexBytes(t, 1)

	// The header file claims a tip height of 1 (2 records: heights 0 and 1),
	// but only the genesis record is written in full - the second record is
	// cut off 10 bytes into its 32-byte hash field.
	path := writeUtxoHeadersFile(t, 1, genesisRecord, secondRecord[:10])

	store := newTestBlockchainStore(t)

	err := processHeaders(context.Background(), ulogger.TestLogger{}, store, path)
	require.Error(t, err, "a truncated utxo-headers file must not be reported as a successful import")
}

// TestLoadCoinbaseTxs_TruncatedFile_ReturnsError is the same regression, for
// loadCoinbaseTxs's independent utxo-headers read loop.
func TestLoadCoinbaseTxs_TruncatedFile_ReturnsError(t *testing.T) {
	firstRecord := dummyBlockIndexBytes(t, 0)
	secondRecord := dummyBlockIndexBytes(t, 1)

	path := writeUtxoHeadersFile(t, 1, firstRecord, secondRecord[:10])

	_, err := loadCoinbaseTxs(ulogger.TestLogger{}, path)
	require.Error(t, err, "a truncated utxo-headers file must not be reported as a successful import")
}

// TestProcessHeaders_CompleteFile_FinishesCleanly is the positive-path
// counterpart to the truncation regression test above: a well-formed,
// complete utxo-headers file (tip height matches the number of records
// actually written) must import cleanly, with every non-genesis header
// stored - guarding against the new record-count validation rejecting a
// legitimate, complete snapshot.
func TestProcessHeaders_CompleteFile_FinishesCleanly(t *testing.T) {
	ctx := context.Background()
	store := newTestBlockchainStore(t)

	genesis, err := store.GetBlockByID(ctx, 0)
	require.NoError(t, err)

	genesisRecord, err := blockToIndexBytes(t, genesis, genesis.Header.Hash())
	require.NoError(t, err)

	coinbase := makeCoinbaseTx(t, 1, 1)
	root := coinbase.TxIDChainHash()

	block1 := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			Timestamp:      1700000001,
			Nonce:          1,
			HashPrevBlock:  genesis.Header.Hash(),
			HashMerkleRoot: root,
			Bits:           genesis.Header.Bits,
		},
		CoinbaseTx:       coinbase,
		TransactionCount: 1,
		Height:           1,
	}
	block1Hash := block1.Header.Hash()

	block1Record, err := blockToIndexBytes(t, block1, block1Hash)
	require.NoError(t, err)

	path := writeUtxoHeadersFile(t, 1, genesisRecord, block1Record)

	require.NoError(t, processHeaders(ctx, ulogger.TestLogger{}, store, path))

	stored, err := store.GetBlockByID(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, block1Hash.String(), stored.Header.Hash().String())
}

// blockToIndexBytes serialises a model.Block into the BlockIndex wire format
// processHeaders/loadCoinbaseTxs read back, mirroring what WriteHeadersToStore
// produces for a real block.
func blockToIndexBytes(t *testing.T, block *model.Block, hash *chainhash.Hash) ([]byte, error) {
	t.Helper()

	bi := &utxopersister.BlockIndex{
		Hash:        hash,
		Height:      block.Height,
		TxCount:     block.TransactionCount,
		BlockHeader: block.Header,
		CoinbaseTx:  block.CoinbaseTx,
	}

	var buf bytes.Buffer
	if err := bi.Serialise(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// TestLoadCoinbaseTxs_CompleteFile_FinishesCleanly is the positive-path
// counterpart to TestLoadCoinbaseTxs_TruncatedFile_ReturnsError: a complete
// V2 utxo-headers file must still yield its coinbase transactions without
// the new record-count validation rejecting it.
func TestLoadCoinbaseTxs_CompleteFile_FinishesCleanly(t *testing.T) {
	store := newTestBlockchainStore(t)
	ctx := context.Background()

	genesis, err := store.GetBlockByID(ctx, 0)
	require.NoError(t, err)

	genesisRecord, err := blockToIndexBytes(t, genesis, genesis.Header.Hash())
	require.NoError(t, err)

	coinbase := makeCoinbaseTx(t, 1, 1)
	root := coinbase.TxIDChainHash()

	block1 := &model.Block{
		Header: &model.BlockHeader{
			Version:        1,
			Timestamp:      1700000001,
			Nonce:          1,
			HashPrevBlock:  genesis.Header.Hash(),
			HashMerkleRoot: root,
			Bits:           genesis.Header.Bits,
		},
		CoinbaseTx:       coinbase,
		TransactionCount: 1,
		Height:           1,
	}
	block1Hash := block1.Header.Hash()

	block1Record, err := blockToIndexBytes(t, block1, block1Hash)
	require.NoError(t, err)

	path := writeUtxoHeadersFile(t, 1, genesisRecord, block1Record)

	coinbaseTxs, err := loadCoinbaseTxs(ulogger.TestLogger{}, path)
	require.NoError(t, err)

	got, ok := coinbaseTxs[*coinbase.TxIDChainHash()]
	require.True(t, ok, "block1's coinbase must be recovered")
	require.Equal(t, coinbase.TxIDChainHash().String(), got.TxIDChainHash().String())
}
