package model

import (
	"testing"

	"github.com/bsv-blockchain/go-bt/v2/chainhash"
	"github.com/stretchr/testify/require"
)

func TestOvertakenTipRepairHoldRoundTrip(t *testing.T) {
	held := []HeldBlock{
		{Hash: chainhash.HashH([]byte("a")), ID: 7, Height: 100},
		{Hash: chainhash.HashH([]byte("b")), ID: 9, Height: 98},
	}

	decoded, err := DecodeOvertakenTipRepairHold(EncodeOvertakenTipRepairHold(held))
	require.NoError(t, err)
	require.Equal(t, held, decoded)
}

func TestOvertakenTipRepairHoldEmpty(t *testing.T) {
	encoded := EncodeOvertakenTipRepairHold(nil)
	require.Empty(t, encoded, "no hold is an empty record")

	decoded, err := DecodeOvertakenTipRepairHold(encoded)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

func TestOvertakenTipRepairHoldRejectsTruncated(t *testing.T) {
	encoded := EncodeOvertakenTipRepairHold([]HeldBlock{{Hash: chainhash.HashH([]byte("a")), ID: 7, Height: 100}})

	_, err := DecodeOvertakenTipRepairHold(encoded[:len(encoded)-1])
	require.Error(t, err)
}
