package subtreevalidation

import (
	"bytes"
	"context"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	subtreepkg "github.com/bsv-blockchain/go-subtree"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/bsv-blockchain/teranode/stores/blob/options"
	"github.com/bsv-blockchain/teranode/stores/utxo"
)

// serializeConflictingTxs encodes the txs kept aside from a peer subtree, in subtree
// order, the way subtreeData encodes transactions.
func serializeConflictingTxs(txs []*bt.Tx) []byte {
	var buf bytes.Buffer

	for _, tx := range txs {
		if tx.IsExtended() {
			buf.Write(tx.ExtendedBytes())
		} else {
			buf.Write(tx.Bytes())
		}
	}

	return buf.Bytes()
}

// readConflictingTxs decodes a FileTypeSubtreeConflicting file.
func readConflictingTxs(b []byte) ([]*bt.Tx, error) {
	reader := bytes.NewReader(b)
	txs := make([]*bt.Tx, 0, 8)

	// bytes remain whenever a read starts, so any error, EOF included, means a truncated file
	for reader.Len() > 0 {
		tx := &bt.Tx{}
		if _, err := tx.ReadFrom(reader); err != nil {
			return nil, errors.NewProcessingError("[readConflictingTxs] failed to read transaction", err)
		}

		txs = append(txs, tx)
	}

	return txs, nil
}

// lookupConflictingTxs resolves missing txs from the txs kept aside next to the subtree,
// if any. A tx only matches the hash it hashes to, so the file cannot substitute one tx
// for another; every tx it supplies is still validated.
func (u *Server) lookupConflictingTxs(ctx context.Context, subtreeHash chainhash.Hash, missingTxHashes []utxo.UnresolvedMetaData) (found []missingTx, stillMissing []utxo.UnresolvedMetaData, err error) {
	exists, err := u.subtreeStore.Exists(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting)
	if err != nil {
		return nil, nil, errors.NewStorageError("[lookupConflictingTxs][%s] failed to check for kept-aside conflicting txs", subtreeHash.String(), err)
	}

	if !exists {
		return nil, missingTxHashes, nil
	}

	// The file only spares a peer request: when it cannot be read, ask the peer as before.
	b, err := u.subtreeStore.Get(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting)
	if err != nil {
		u.logger.Warnf("[lookupConflictingTxs][%s] ignoring kept-aside conflicting txs that could not be read: %v", subtreeHash.String(), err)
		return nil, missingTxHashes, nil
	}

	txs, err := readConflictingTxs(b)
	if err != nil {
		u.logger.Warnf("[lookupConflictingTxs][%s] ignoring unreadable kept-aside conflicting txs: %v", subtreeHash.String(), err)
		return nil, missingTxHashes, nil
	}

	byHash := make(map[chainhash.Hash]*bt.Tx, len(txs))
	for _, tx := range txs {
		byHash[*tx.TxIDChainHash()] = tx
	}

	for _, h := range missingTxHashes {
		if tx, ok := byHash[h.Hash]; ok {
			found = append(found, missingTx{tx: tx, idx: h.Idx})
		} else {
			stillMissing = append(stillMissing, h)
		}
	}

	return found, stillMissing, nil
}

// subtreeConflictingTxs returns the txs kept aside next to subtree, if there are any and
// every one of them is a node of subtree. Otherwise ok is false and the caller loads the
// subtree's txs the usual way.
func (u *Server) subtreeConflictingTxs(ctx context.Context, subtree *subtreepkg.Subtree) (txs []*bt.Tx, ok bool, err error) {
	subtreeHash := subtree.RootHash()

	exists, err := u.subtreeStore.Exists(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting)
	if err != nil {
		return nil, false, errors.NewStorageError("[subtreeConflictingTxs][%s] failed to check for kept-aside conflicting txs", subtreeHash.String(), err)
	}

	if !exists {
		return nil, false, nil
	}

	b, err := u.subtreeStore.Get(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting)
	if err != nil {
		return nil, false, errors.NewStorageError("[subtreeConflictingTxs][%s] failed to read kept-aside conflicting txs", subtreeHash.String(), err)
	}

	txs, err = readConflictingTxs(b)
	if err != nil {
		u.logger.Warnf("[subtreeConflictingTxs][%s] ignoring unreadable kept-aside conflicting txs: %v", subtreeHash.String(), err)
		return nil, false, nil
	}

	pending := make(map[chainhash.Hash]struct{}, len(txs))
	for _, tx := range txs {
		pending[*tx.TxIDChainHash()] = struct{}{}
	}

	for _, node := range subtree.Nodes {
		delete(pending, node.Hash)
	}

	if len(pending) > 0 {
		u.logger.Warnf("[subtreeConflictingTxs][%s] ignoring kept-aside conflicting txs: %d are not in the subtree", subtreeHash.String(), len(pending))
		return nil, false, nil
	}

	return txs, true, nil
}

// checkKeptAsideSubtreeNodes runs the node checks a validated subtree gets before it is
// stored, for a peer subtree stored with kept-aside txs: no duplicate tx, the coinbase
// placeholder only at index 0, and every tx that was not kept aside validated.
func checkKeptAsideSubtreeNodes(subtreeHash chainhash.Hash, txHashes []chainhash.Hash, txMetaSlice []metaSliceItem) error {
	seenSet := getSeenSet(len(txHashes))
	defer putSeenSet(seenSet)

	for idx, txHash := range txHashes {
		if idx == 0 && txHash.Equal(*subtreepkg.CoinbasePlaceholderHash) {
			continue
		}

		if _, dup := seenSet.m[txHash]; dup {
			return errors.NewBlockCorruptError("[ValidateSubtreeInternal][%s] duplicate transaction in subtree at index %d: %s", subtreeHash.String(), idx, txHash.String())
		}

		seenSet.m[txHash] = struct{}{}

		if txMetaSlice[idx].keptAside {
			continue
		}

		if !txMetaSlice[idx].isSet {
			return errors.NewProcessingError("[ValidateSubtreeInternal][%s] tx meta not found in txMetaSlice at index %d: %s", subtreeHash.String(), idx, txHash.String())
		}

		if txMetaSlice[idx].coinbase {
			return errors.NewTxInvalidError("[ValidateSubtreeInternal][%s] invalid subtree index for coinbase tx %d: %s", subtreeHash.String(), idx, txHash.String())
		}
	}

	return nil
}

// subtreeToCheckFromHashes builds the FileTypeSubtreeToCheck form of a subtree from its
// node hashes and checks it against the subtree's root.
func subtreeToCheckFromHashes(subtreeHash chainhash.Hash, txHashes []chainhash.Hash) (*subtreepkg.Subtree, []byte, error) {
	subtreeToCheck, err := subtreepkg.NewIncompleteTreeByLeafCount(len(txHashes))
	if err != nil {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromHashes][%s] failed to create subtree structure", subtreeHash.String(), err)
	}

	for _, nodeHash := range txHashes {
		if nodeHash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
			if err = subtreeToCheck.AddCoinbaseNode(); err != nil {
				return nil, nil, errors.NewProcessingError("[subtreeToCheckFromHashes][%s] failed to add coinbase node to subtree", subtreeHash.String(), err)
			}
		} else if err = subtreeToCheck.AddNode(nodeHash, 0, 0); err != nil {
			return nil, nil, errors.NewProcessingError("[subtreeToCheckFromHashes][%s] failed to add node to subtree", subtreeHash.String(), err)
		}
	}

	if !subtreeHash.Equal(*subtreeToCheck.RootHash()) {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromHashes][%s] subtree root hash mismatch: %s", subtreeHash.String(), subtreeToCheck.RootHash().String())
	}

	subtreeBytes, err := subtreeToCheck.Serialize()
	if err != nil {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromHashes][%s] failed to serialize subtree", subtreeHash.String(), err)
	}

	return subtreeToCheck, subtreeBytes, nil
}

// storeSubtreeWithConflictingTxs stores a peer subtree that has txs kept aside. It is
// never stored as FileTypeSubtree: readers trust that file to have every tx in the UTXO
// store. The kept-aside txs are stored first and the subtree as FileTypeSubtreeToCheck
// second, so a subtree to check from this path always has its kept-aside txs next to it.
// The block that references the subtree validates it, kept-aside txs included.
func (u *Server) storeSubtreeWithConflictingTxs(ctx context.Context, subtreeHash chainhash.Hash, txHashes []chainhash.Hash, keptAside []*bt.Tx) error {
	_, subtreeBytes, err := subtreeToCheckFromHashes(subtreeHash, txHashes)
	if err != nil {
		return err
	}

	dah := u.utxoStore.GetBlockHeight() + u.settings.GetSubtreeValidationBlockHeightRetention()

	if err = u.subtreeStore.Set(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting, serializeConflictingTxs(keptAside), options.WithDeleteAt(dah)); err != nil && !errors.Is(err, errors.ErrBlobAlreadyExists) {
		return errors.NewStorageError("[storeSubtreeWithConflictingTxs][%s] failed to store conflicting txs", subtreeHash.String(), err)
	}

	if err = u.subtreeStore.Set(ctx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes, options.WithDeleteAt(dah)); err != nil && !errors.Is(err, errors.ErrBlobAlreadyExists) {
		return errors.NewStorageError("[storeSubtreeWithConflictingTxs][%s] failed to store subtree to check", subtreeHash.String(), err)
	}

	prometheusSubtreeValidationConflictingKeptAside.Add(float64(len(keptAside)))

	u.logger.Infof("[storeSubtreeWithConflictingTxs][%s] kept %d conflicting txs aside until a block references the subtree", subtreeHash.String(), len(keptAside))

	return nil
}
