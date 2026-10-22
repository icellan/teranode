package validator

import (
	"net/url"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-bt/v2"
	bec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/teranode/errors"
	utxostore "github.com/bsv-blockchain/teranode/stores/utxo"
	"github.com/bsv-blockchain/teranode/stores/utxo/sql"
	"github.com/bsv-blockchain/teranode/test/utils/transactions"
	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/bsv-blockchain/teranode/util/kafka"
	kafkamessage "github.com/bsv-blockchain/teranode/util/kafka/kafka_message"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestConflictingRejectionIsPublished pins the source of the subtree validators'
// conflict knowledge: a transaction the validator rejects because one of its inputs
// is already spent by a different transaction is published, raw, on the
// policy-rejected topic with Conflicting set. Subtree validation uses it to keep such
// a transaction aside (no UTXO-store write) when a peer subtree contains it.
func TestConflictingRejectionIsPublished(t *testing.T) {
	newFixture := func(t *testing.T, name string) (*Validator, *kafka.KafkaAsyncProducerMock, *bt.Tx, *bt.Tx) {
		ctx := t.Context()
		logger := ulogger.NewErrorTestLogger(t)
		tSettings := test.CreateBaseTestSettings(t)
		tSettings.BlockAssembly.Disabled = true

		utxoStoreURL, err := url.Parse("sqlitememory:///" + name)
		require.NoError(t, err)

		utxoStore, err := sql.New(ctx, logger, tSettings, utxoStoreURL)
		require.NoError(t, err)
		utxoStore.RawDB().SetMaxOpenConns(1)
		require.NoError(t, utxoStore.SetBlockHeight(100))
		require.NoError(t, utxoStore.SetMedianBlockTime(uint32(time.Now().Unix()))) //nolint:gosec

		privateKey, publicKey := bec.PrivateKeyFromBytes([]byte("CONFLICTING_PUBLISH_TEST_KEY!!!!"))
		parentTx := transactions.Create(t,
			transactions.WithCoinbaseData(1, "/conflicting publish test/"),
			transactions.WithP2PKHOutputs(1, 100_000, publicKey),
		)
		_, err = utxoStore.Create(ctx, parentTx, 1, utxostore.WithMinedBlockInfo(utxostore.MinedBlockInfo{BlockID: 1, BlockHeight: 1, SubtreeIdx: 0}))
		require.NoError(t, err)

		winner := transactions.Create(t,
			transactions.WithPrivateKey(privateKey),
			transactions.WithInput(parentTx, 0, privateKey),
			transactions.WithP2PKHOutputs(1, 90_000, publicKey),
		)
		loser := transactions.Create(t,
			transactions.WithPrivateKey(privateKey),
			transactions.WithInput(parentTx, 0, privateKey),
			transactions.WithP2PKHOutputs(1, 80_000, publicKey),
		)

		producer := kafka.NewKafkaAsyncProducerMock()

		validator, err := New(ctx, logger, tSettings, utxoStore, nil, nil, producer, nil, nil)
		require.NoError(t, err)

		v := validator.(*Validator)

		_, err = v.Validate(ctx, winner, 100)
		require.NoError(t, err)

		return v, producer, winner, loser
	}

	t.Run("rejected conflicting tx is published with Conflicting set", func(t *testing.T) {
		v, producer, _, loser := newFixture(t, "conflicting_rejected_published")

		txMeta, err := v.Validate(t.Context(), loser, 100)
		require.Error(t, err)
		require.True(t, errors.Is(err, errors.ErrSpent), "the rejection must be a spend conflict: %v", err)
		require.Nil(t, txMeta, "a rejected conflicting tx is not stored")

		select {
		case msg := <-producer.PublishChannel():
			var m kafkamessage.KafkaTxPolicyRejectedTopicMessage
			require.NoError(t, proto.Unmarshal(msg.Value, &m))
			require.True(t, m.Conflicting)
			require.Equal(t, loser.TxIDChainHash().CloneBytes(), m.TxHash)
			require.Equal(t, loser.SerializeBytes(), m.RawTx)
		default:
			t.Fatal("expected the rejected conflicting tx to be published")
		}
	})

	t.Run("tx created as conflicting is not published", func(t *testing.T) {
		v, producer, _, loser := newFixture(t, "conflicting_rejected_created")

		txMeta, err := v.Validate(t.Context(), loser, 100, WithCreateConflicting(true))
		require.True(t, errors.Is(err, errors.ErrTxConflicting), "block-path create must return conflicting: %v", err)
		require.NotNil(t, txMeta)
		require.True(t, txMeta.Conflicting)

		select {
		case msg := <-producer.PublishChannel():
			t.Fatalf("a tx stored as conflicting must not be published, got %d bytes", len(msg.Value))
		default:
		}
	})
}
