package legacy

import (
	"testing"

	"github.com/bsv-blockchain/teranode/settings"
	"github.com/bsv-blockchain/teranode/util/test/mocklogger"
	"github.com/stretchr/testify/require"
)

// TestResolveAdminAPIKey_Configured verifies that a configured admin API key
// is returned verbatim and no warning is logged.
func TestResolveAdminAPIKey_Configured(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	s := &Server{
		logger:   logger,
		settings: &settings.Settings{GRPCAdminAPIKey: "configured-key"},
	}

	apiKey := s.resolveAdminAPIKey()

	require.Equal(t, "configured-key", apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 0)
}

// TestResolveAdminAPIKey_Empty verifies that an unset admin API key is
// returned as-is (no key is fabricated - a generated key no client could
// ever learn just masked the exposure) and a single warning is logged
// naming the exposure.
func TestResolveAdminAPIKey_Empty(t *testing.T) {
	logger := mocklogger.NewTestLogger()
	s := &Server{
		logger:   logger,
		settings: &settings.Settings{GRPCAdminAPIKey: ""},
	}

	apiKey := s.resolveAdminAPIKey()

	require.Empty(t, apiKey)
	logger.AssertNumberOfCalls(t, "Warnf", 1)
}
