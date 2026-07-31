package s3

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/bsv-blockchain/teranode/errors"
	"github.com/bsv-blockchain/teranode/pkg/fileformat"
	"github.com/stretchr/testify/require"
)

// missingKeyErrors are the ways a missing object surfaces from the AWS SDK.
//
// Exists() already handles all of these — it carries the workaround for
// aws/aws-sdk-go-v2#2084, where a 404 is rendered as NotFound rather than
// NoSuchKey — but the read paths only ever matched the literal string
// "NoSuchKey". That gap matters because callers distinguish "this object is not
// here" from "the store failed": stores/utxo/aerospike falls back to the
// .outputs blob only on a genuine miss, so an unrecognised miss reads as a
// storage failure and aborts an operation that should have succeeded.
func missingKeyErrors() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{name: "typed NoSuchKey", err: &types.NoSuchKey{}},
		{name: "typed NotFound", err: &types.NotFound{}},
		{
			// The shape reported in aws/aws-sdk-go-v2#2084: a generic API error
			// whose code is NotFound and whose message never says NoSuchKey.
			name: "generic NotFound api error",
			err:  &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"},
		},
	}
}

func TestS3_GetIoReader_MissingKeyMapsToErrNotFound(t *testing.T) {
	for _, tt := range missingKeyErrors() {
		t.Run(tt.name, func(t *testing.T) {
			s3Store, mock := setupTestS3(t)
			mock.getObjectMissErr = tt.err

			_, err := s3Store.GetIoReader(context.Background(), []byte("missing"), fileformat.FileTypeTx)

			require.Error(t, err)
			require.True(t, errors.Is(err, errors.ErrNotFound),
				"a missing object must map to ErrNotFound so callers can tell a miss from a store failure, got %v", err)
		})
	}
}

func TestS3_Get_MissingKeyMapsToErrNotFound(t *testing.T) {
	for _, tt := range missingKeyErrors() {
		t.Run(tt.name, func(t *testing.T) {
			s3Store, mock := setupTestS3(t)
			mock.getObjectMissErr = tt.err

			_, err := s3Store.Get(context.Background(), []byte("missing"), fileformat.FileTypeTx)

			require.Error(t, err)
			require.True(t, errors.Is(err, errors.ErrNotFound),
				"a missing object must map to ErrNotFound so callers can tell a miss from a store failure, got %v", err)
		})
	}
}

// TestS3_GetIoReader_RealFailureIsNotAMiss is the other half of the contract: a
// genuine store failure must NOT be reported as a miss, or a caller that treats
// not-found as "fall back to the degraded path" will silently mask an outage.
func TestS3_GetIoReader_RealFailureIsNotAMiss(t *testing.T) {
	s3Store, mock := setupTestS3(t)
	mock.getObjectMissErr = &smithy.GenericAPIError{Code: "InternalError", Message: "We encountered an internal error"}

	_, err := s3Store.GetIoReader(context.Background(), []byte("missing"), fileformat.FileTypeTx)

	require.Error(t, err)
	require.False(t, errors.Is(err, errors.ErrNotFound),
		"an internal store error must not be reported as a missing object")
}
