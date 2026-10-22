package validator

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/util/test"
	"github.com/stretchr/testify/require"
)

func TestCheckTransactionContextFree(t *testing.T) {
	policy := test.CreateBaseTestSettings(t).Policy

	valid := func(t *testing.T) *bt.Tx {
		tx := newPolicyRejectedTestTx(t, 32)
		require.NoError(t, CheckTransactionContextFree(tx, policy))

		return tx
	}

	t.Run("a well-formed tx passes", func(t *testing.T) {
		valid(t)
	})

	t.Run("coinbase", func(t *testing.T) {
		tx := bt.NewTx()
		require.NoError(t, tx.From("0000000000000000000000000000000000000000000000000000000000000000", 0xffffffff, "", 0))
		require.NoError(t, tx.AddOpReturnOutput([]byte{1}))
		require.True(t, tx.IsCoinbase())
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("no inputs", func(t *testing.T) {
		tx := valid(t)
		tx.Inputs = nil
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("no outputs", func(t *testing.T) {
		tx := valid(t)
		tx.Outputs = nil
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("duplicate input", func(t *testing.T) {
		tx := valid(t)
		dup := *tx.Inputs[0]
		tx.Inputs = append(tx.Inputs, &dup)
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("null prevout", func(t *testing.T) {
		tx := valid(t)
		null := bt.NewTx()
		require.NoError(t, null.From("0000000000000000000000000000000000000000000000000000000000000000", 0xffffffff, "", 0))
		tx.Inputs = append(tx.Inputs, null.Inputs[0])
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("output value too large", func(t *testing.T) {
		tx := valid(t)
		tx.Outputs[0].Satoshis = MaxSatoshis + 1
		require.True(t, errors.Is(CheckTransactionContextFree(tx, policy), errors.ErrTxInvalid))
	})

	t.Run("larger than the policy max tx size", func(t *testing.T) {
		tx := valid(t)
		small := *policy
		small.MaxTxSizePolicy = tx.Size() - 1
		require.True(t, errors.Is(CheckTransactionContextFree(tx, &small), errors.ErrTxPolicy))
	})
}
