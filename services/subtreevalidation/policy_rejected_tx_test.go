package subtreevalidation

import (
	"context"
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestTxPolicyRejectedCache_SetAndGet(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	tx, err := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	require.NoError(t, err)

	hash := *tx.TxIDChainHash()

	cache.Set(hash, tx)

	got, ok := cache.Get(hash)
	require.True(t, ok)
	assert.Equal(t, tx.TxID(), got.TxID())
}

func TestTxPolicyRejectedCache_MissReturnsNotFound(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	var fakeHash chainhash.Hash
	fakeHash[0] = 0xAB

	_, ok := cache.Get(fakeHash)
	require.False(t, ok)
}

func TestTxPolicyRejectedCache_EvictsWhenFull(t *testing.T) {
	tx1, _ := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	tx2, _ := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")

	// tx1 and tx2 are identical in size; bound the cache to exactly two of them.
	cache := newTxPolicyRejectedCache(2 * tx1.Size())

	var hash1, hash2, hash3 chainhash.Hash
	hash1[0] = 1
	hash2[0] = 2
	hash3[0] = 3

	cache.Set(hash1, tx1)
	cache.Set(hash2, tx2)
	require.Equal(t, 2, cache.Len())
	require.Equal(t, 2*tx1.Size(), cache.Bytes())

	cache.Set(hash3, tx1)
	require.Equal(t, 2, cache.Len(), "cache should evict one entry to stay within byte budget")
	require.LessOrEqual(t, cache.Bytes(), 2*tx1.Size(), "byte usage must stay within budget")
}

// TestTxPolicyRejectedCache_BoundedByBytes is the regression guard for the eviction
// bound being on cumulative tx bytes rather than entry count: a flood of distinct
// transactions must never push memory usage past the configured byte budget.
func TestTxPolicyRejectedCache_BoundedByBytes(t *testing.T) {
	tx, _ := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	txSize := tx.Size()

	budget := 5 * txSize
	cache := newTxPolicyRejectedCache(budget)

	for i := 0; i < 1000; i++ {
		var h chainhash.Hash
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		cache.Set(h, tx)

		require.LessOrEqual(t, cache.Bytes(), budget, "cache must never exceed the byte budget")
	}

	require.LessOrEqual(t, cache.Len(), 5, "entry count is implied by the byte budget, not assumed")
}

func TestLookupPolicyRejectedTxs(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	tx, err := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	require.NoError(t, err)

	var cachedHash chainhash.Hash
	cachedHash[0] = 0xAA
	cache.Set(cachedHash, tx)

	var missHash chainhash.Hash
	missHash[0] = 0xBB

	server := &Server{
		policyRejectedTxCache: cache,
	}

	toCheck := []missingTxHash{
		{hash: cachedHash, idx: 0},
		{hash: missHash, idx: 1},
	}

	found, still := server.lookupPolicyRejectedTxs(toCheck)

	require.Len(t, found, 1)
	assert.Equal(t, 0, found[0].idx)
	assert.Equal(t, tx.TxID(), found[0].tx.TxID())

	require.Len(t, still, 1)
	assert.Equal(t, missHash, still[0].hash)
	assert.Equal(t, 1, still[0].idx)
}

func TestLookupPolicyRejectedTxs_NilCache(t *testing.T) {
	server := &Server{
		policyRejectedTxCache: nil,
	}

	input := []missingTxHash{{hash: chainhash.Hash{}, idx: 0}}

	found, still := server.lookupPolicyRejectedTxs(input)
	assert.Nil(t, found)
	assert.Equal(t, input, still)
}

func TestPolicyRejectedTxMessageHandler(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	txHex := "01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000"
	tx, err := bt.NewTxFromString(txHex)
	require.NoError(t, err)

	txHash := tx.TxIDChainHash()

	m := &kafkamessage.KafkaTxPolicyRejectedTopicMessage{
		TxHash: txHash.CloneBytes(),
		RawTx:  tx.SerializeBytes(),
		Reason: "insufficient fee",
	}

	value, err := proto.Marshal(m)
	require.NoError(t, err)

	server := &Server{
		policyRejectedTxCache: cache,
		logger:                ulogger.New("test"),
	}

	handler := server.policyRejectedTxMessageHandler(context.Background())

	err = handler(&kafka.KafkaMessage{
		Value: value,
	})
	require.NoError(t, err)

	got, ok := cache.Get(*txHash)
	require.True(t, ok)
	assert.Equal(t, tx.TxID(), got.TxID())
}

// TestTxPolicyRejectedCache_Conflicting pins that the cache tells a transaction the
// validator rejected as conflicting apart from a policy-rejected one: subtree validation
// keeps only the former aside instead of failing the subtree.
func TestTxPolicyRejectedCache_Conflicting(t *testing.T) {
	tx, err := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	require.NoError(t, err)

	var policyHash, conflictingHash, upgradedHash chainhash.Hash
	policyHash[0] = 1
	conflictingHash[0] = 2
	upgradedHash[0] = 3

	cache := newTxPolicyRejectedCache(1024 * 1024)

	cache.Set(policyHash, tx)
	cache.SetConflicting(conflictingHash, tx)

	_, ok := cache.Conflicting(policyHash)
	require.False(t, ok, "a policy-rejected tx is not conflicting")

	got, ok := cache.Conflicting(conflictingHash)
	require.True(t, ok)
	require.Equal(t, tx.TxID(), got.TxID())

	_, ok = cache.Get(conflictingHash)
	require.True(t, ok, "a conflicting tx still serves the missing-tx lookup")

	// A tx first cached as policy-rejected and later rejected as conflicting is upgraded,
	// without counting its bytes twice.
	cache.Set(upgradedHash, tx)
	bytesBefore := cache.Bytes()

	cache.SetConflicting(upgradedHash, tx)

	_, ok = cache.Conflicting(upgradedHash)
	require.True(t, ok)
	require.Equal(t, bytesBefore, cache.Bytes())
}

func TestPolicyRejectedTxMessageHandler_Conflicting(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	tx, err := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	require.NoError(t, err)

	value, err := proto.Marshal(&kafkamessage.KafkaTxPolicyRejectedTopicMessage{
		TxHash:      tx.TxIDChainHash().CloneBytes(),
		RawTx:       tx.SerializeBytes(),
		Reason:      "utxo already spent",
		Conflicting: true,
	})
	require.NoError(t, err)

	server := &Server{
		policyRejectedTxCache: cache,
		logger:                ulogger.New("test"),
	}

	require.NoError(t, server.policyRejectedTxMessageHandler(context.Background())(&kafka.KafkaMessage{Value: value}))

	got, ok := cache.Conflicting(*tx.TxIDChainHash())
	require.True(t, ok)
	require.Equal(t, tx.TxID(), got.TxID())
}

func TestPolicyRejectedTxMessageHandler_InvalidProto(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	server := &Server{
		policyRejectedTxCache: cache,
		logger:                ulogger.New("test"),
	}

	handler := server.policyRejectedTxMessageHandler(context.Background())

	err := handler(&kafka.KafkaMessage{
		Value: []byte("not-a-proto"),
	})
	require.NoError(t, err, "handler should swallow proto errors")
	assert.Equal(t, 0, cache.Len())
}

func TestPolicyRejectedTxMessageHandler_EmptyRawTx(t *testing.T) {
	cache := newTxPolicyRejectedCache(1024 * 1024)

	m := &kafkamessage.KafkaTxPolicyRejectedTopicMessage{
		TxHash: make([]byte, 32),
		RawTx:  nil,
		Reason: "test",
	}

	value, _ := proto.Marshal(m)

	server := &Server{
		policyRejectedTxCache: cache,
		logger:                ulogger.New("test"),
	}

	handler := server.policyRejectedTxMessageHandler(context.Background())

	err := handler(&kafka.KafkaMessage{Value: value})
	require.NoError(t, err)
	assert.Equal(t, 0, cache.Len(), "should skip messages with empty rawTx")
}

// TestPolicyRejectedTxMessageHandler_KeptAsideHasItsOwnBudget pins the flood guard:
// descendants a subtree validator kept aside are only context-free checked, so they get
// their own byte budget and cannot evict the txs the validators rejected.
func TestPolicyRejectedTxMessageHandler_KeptAsideHasItsOwnBudget(t *testing.T) {
	tx, err := bt.NewTxFromString("01000000010000000000000000000000000000000000000000000000000000000000000000ffffffff0704ffff001d0104ffffffff0100f2052a0100000043410496b538e853519c726a2c91e61ec11600ae1390813a627c66fb8be7947be63c52da7589379515d4e0a604f8141781e62294721166bf621e73a82cbf2342c858eeac00000000")
	require.NoError(t, err)

	server := &Server{
		policyRejectedTxCache: newTxPolicyRejectedCache(2 * tx.Size()),
		keptAsideTxCache:      newTxPolicyRejectedCache(2 * tx.Size()),
		logger:                ulogger.New("test"),
	}

	handler := server.policyRejectedTxMessageHandler(context.Background())

	publish := func(tx *bt.Tx, conflicting, keptAside bool) {
		value, err := proto.Marshal(&kafkamessage.KafkaTxPolicyRejectedTopicMessage{
			TxHash: tx.TxIDChainHash().CloneBytes(), RawTx: tx.SerializeBytes(), Conflicting: conflicting, KeptAside: keptAside,
		})
		require.NoError(t, err)
		require.NoError(t, handler(&kafka.KafkaMessage{Value: value}))
	}

	real := *tx.TxIDChainHash()
	publish(tx, true, false)

	// a flood of distinct kept-aside descendants, each the same size as the real entry
	for i := 0; i < 50; i++ {
		variant := tx.Clone()
		variant.Outputs[0].Satoshis = uint64(1000 + i)
		require.Equal(t, tx.Size(), variant.Size())
		publish(variant, true, true)
	}

	_, ok := server.policyRejectedTxCache.Conflicting(real)
	require.True(t, ok, "the validator's entry survives a flood of kept-aside entries")
	require.LessOrEqual(t, server.keptAsideTxCache.Bytes(), 2*tx.Size())

	_, ok = server.conflictingTx(real)
	require.True(t, ok)
}
