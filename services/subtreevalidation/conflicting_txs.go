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
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"google.golang.org/protobuf/proto"
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

	// A storage error halts; unreadable content only spares a peer request, so then the
	// peer is asked as before.
	b, err := u.subtreeStore.Get(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting)
	if err != nil {
		return nil, nil, errors.NewStorageError("[lookupConflictingTxs][%s] failed to read kept-aside conflicting txs", subtreeHash.String(), err)
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

// incompleteTreeFromNodes builds a subtree of leafCount nodes from their hashes, node(i)
// returning the i-th, with the coinbase placeholder added as the coinbase node.
func incompleteTreeFromNodes(leafCount int, node func(i int) chainhash.Hash) (*subtreepkg.Subtree, error) {
	tree, err := subtreepkg.NewIncompleteTreeByLeafCount(leafCount)
	if err != nil {
		return nil, errors.NewProcessingError("failed to create subtree structure", err)
	}

	for i := 0; i < leafCount; i++ {
		nodeHash := node(i)

		if nodeHash.Equal(subtreepkg.CoinbasePlaceholderHashValue) {
			if err = tree.AddCoinbaseNode(); err != nil {
				return nil, errors.NewProcessingError("failed to add coinbase node to subtree", err)
			}
		} else if err = tree.AddNode(nodeHash, 0, 0); err != nil {
			return nil, errors.NewProcessingError("failed to add node to subtree", err)
		}
	}

	return tree, nil
}

// subtreeToCheckFromNodes builds the FileTypeSubtreeToCheck form of a subtree from its
// leafCount node hashes, node(i) returning the i-th, and checks it against the subtree's root.
func subtreeToCheckFromNodes(subtreeHash chainhash.Hash, leafCount int, node func(i int) chainhash.Hash) (*subtreepkg.Subtree, []byte, error) {
	subtreeToCheck, err := incompleteTreeFromNodes(leafCount, node)
	if err != nil {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromNodes][%s] failed to build subtree", subtreeHash.String(), err)
	}

	if !subtreeHash.Equal(*subtreeToCheck.RootHash()) {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromNodes][%s] subtree root hash mismatch: %s", subtreeHash.String(), subtreeToCheck.RootHash().String())
	}

	subtreeBytes, err := subtreeToCheck.Serialize()
	if err != nil {
		return nil, nil, errors.NewProcessingError("[subtreeToCheckFromNodes][%s] failed to serialize subtree", subtreeHash.String(), err)
	}

	return subtreeToCheck, subtreeBytes, nil
}

// storeSubtreeWithConflictingTxs stores a peer subtree that has txs kept aside. It is
// never stored as FileTypeSubtree: readers trust that file to have every tx in the UTXO
// store. The kept-aside txs are stored first and the subtree as FileTypeSubtreeToCheck
// second, so a subtree to check from this path always has its kept-aside txs next to it.
// A kept-aside file left by a pass that stopped before the subtree to check is replaced:
// this pass may have kept a different set aside. The block that references the subtree
// validates it, kept-aside txs included.
//
// Only the kept-aside txs the peer sent are written. Those this node resolved from its own
// caches are resolved again when the block needs them; writing them would let a peer fill
// the subtree store with txs it never sent, at 32 bytes a tx. Over the per-subtree cap the
// subtree fails, as it did before txs were kept aside.
func (u *Server) storeSubtreeWithConflictingTxs(ctx context.Context, subtreeHash chainhash.Hash, txHashes []chainhash.Hash, keptAside []missingTx) error {
	_, subtreeBytes, err := subtreeToCheckFromNodes(subtreeHash, len(txHashes), func(i int) chainhash.Hash { return txHashes[i] })
	if err != nil {
		return err
	}

	sent := make([]*bt.Tx, 0, len(keptAside))
	all := make([]*bt.Tx, 0, len(keptAside))
	sentBytes := 0

	for _, mTx := range keptAside {
		all = append(all, mTx.tx)

		if !mTx.cached {
			sent = append(sent, mTx.tx)
			sentBytes += mTx.tx.Size()
		}
	}

	if maxBytes := u.settings.SubtreeValidation.KeptAsideMaxBytesPerSubtree; sentBytes > maxBytes {
		return errors.NewProcessingError("[storeSubtreeWithConflictingTxs][%s] %d bytes of conflicting txs kept aside, over the %d byte limit", subtreeHash.String(), sentBytes, maxBytes)
	}

	// A kept-aside tx is not fully validated, so the previous-output data a peer attached to
	// it in extended format was never checked; it is dropped before the tx is kept anywhere.
	// The block path validates the plain tx, reading its parents from the UTXO store.
	sent, err = withoutExtendedData(sent)
	if err != nil {
		return errors.NewProcessingError("[storeSubtreeWithConflictingTxs][%s] failed to drop extended data of kept-aside txs", subtreeHash.String(), err)
	}

	all, err = withoutExtendedData(all)
	if err != nil {
		return errors.NewProcessingError("[storeSubtreeWithConflictingTxs][%s] failed to drop extended data of kept-aside txs", subtreeHash.String(), err)
	}

	dah := u.utxoStore.GetBlockHeight() + u.settings.GetSubtreeValidationBlockHeightRetention()

	if err = u.subtreeStore.Set(ctx, subtreeHash[:], fileformat.FileTypeSubtreeConflicting, serializeConflictingTxs(sent), options.WithDeleteAt(dah), options.WithAllowOverwrite(true)); err != nil {
		return errors.NewStorageError("[storeSubtreeWithConflictingTxs][%s] failed to store conflicting txs", subtreeHash.String(), err)
	}

	if err = u.subtreeStore.Set(ctx, subtreeHash[:], fileformat.FileTypeSubtreeToCheck, subtreeBytes, options.WithDeleteAt(dah)); err != nil && !errors.Is(err, errors.ErrBlobAlreadyExists) {
		return errors.NewStorageError("[storeSubtreeWithConflictingTxs][%s] failed to store subtree to check", subtreeHash.String(), err)
	}

	u.shareConflictingTxs(all)

	prometheusSubtreeValidationConflictingKeptAside.Add(float64(len(keptAside)))

	u.logger.Infof("[storeSubtreeWithConflictingTxs][%s] kept %d conflicting txs aside until a block references the subtree, %d of them sent by the peer", subtreeHash.String(), len(keptAside), len(sent))

	return nil
}

// withoutExtendedData returns txs in plain format, without the previous output data an
// extended tx carries.
func withoutExtendedData(txs []*bt.Tx) ([]*bt.Tx, error) {
	plain := make([]*bt.Tx, len(txs))

	for i, tx := range txs {
		p, err := bt.NewTxFromBytes(tx.Bytes())
		if err != nil {
			return nil, err
		}

		plain[i] = p
	}

	return plain, nil
}

// shareConflictingTxs makes kept-aside txs that are not yet known as conflicting known:
// cached here at once in the kept-aside cache, and published on the policy-rejected topic,
// flagged keptAside, for every other subtree validator. Mostly these are descendants; a
// loser whose validator publish this pod has not consumed yet goes out again. A consumer
// that already has the validator's message skips it, and one that gets it first counts
// the tx once when both arrive. The publish is best-effort, like the validators': a
// dropped message only means a child in another subtree fails that subtree, as before.
func (u *Server) shareConflictingTxs(keptAside []*bt.Tx) {
	for _, tx := range keptAside {
		hash := *tx.TxIDChainHash()

		if u.knownConflicting(hash) {
			continue
		}

		// the cap the consumers apply, here too: one large tx would evict every other entry
		rawTx := tx.SerializeBytes()
		if len(rawTx) > maxCachedTxBytes {
			continue
		}

		if u.keptAsideTxCache != nil {
			u.keptAsideTxCache.SetConflicting(hash, tx)
		}

		if u.conflictingTxKafkaProducer == nil {
			continue
		}

		value, err := proto.Marshal(&kafkamessage.KafkaTxPolicyRejectedTopicMessage{
			TxHash:      hash.CloneBytes(),
			RawTx:       rawTx,
			Reason:      "kept aside: spends a conflicting tx",
			Conflicting: true,
			KeptAside:   true,
		})
		if err != nil {
			u.logger.Errorf("[shareConflictingTxs][%s] failed to marshal kept-aside conflicting tx: %v", hash.String(), err)
			continue
		}

		if !u.conflictingTxKafkaProducer.TryPublish(&kafka.Message{Key: hash.CloneBytes(), Value: value}) {
			u.logger.Debugf("[shareConflictingTxs][%s] dropped kept-aside conflicting tx: producer buffer full", hash.String())
		}
	}
}
