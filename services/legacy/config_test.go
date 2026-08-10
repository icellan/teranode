package legacy

import (
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bsv-blockchain/teranode/ulogger"
	"github.com/stretchr/testify/assert"
)

func TestExcessiveBlockSizeUserAgentComment(t *testing.T) {
	// Wipe test args.
	os.Args = []string{"bsvd"}

	cfg, _, err := loadConfig(ulogger.TestLogger{})
	if err != nil {
		t.Fatal("Failed to load configuration")
	}

	if len(cfg.UserAgentComments) != 1 {
		t.Fatal("Expected EB UserAgentComment")
	}

	// uac := cfg.UserAgentComments[0]
	// uacExpected := "EB32.0"
	// if uac != uacExpected {
	//	t.Fatalf("Expected UserAgentComments to contain %s but got %s", uacExpected, uac)
	// }

	// Custom excessive block size.
	os.Args = []string{"bsvd", "--excessiveblocksize=64000000"}

	cfg, _, err = loadConfig(ulogger.TestLogger{})
	if err != nil {
		t.Fatal("Failed to load configuration")
	}

	if len(cfg.UserAgentComments) != 1 {
		t.Fatal("Expected EB UserAgentComment")
	}
	// uac = cfg.UserAgentComments[0]
	// uacExpected = "EB64.0"
	// we do not support the command line options an
	//
	//	if uac != uacExpected {
	//		t.Fatalf("Expected UserAgentComments to contain %s but got %s", uacExpected, uac)
	//	}
}

// TestClampExcessiveBlockSizeRespectsValueBelowWireCapacity confirms a
// configured ExcessiveBlockSize below the wire protocol's actual relay
// capacity is respected as-is, rather than being floored up to some other
// value.
//
// Note: this exercises clampExcessiveBlockSize directly rather than going
// through loadConfig's os.Args-based flag parsing. That CLI parsing path is
// already dead code in this file (the flags.NewIniParser/parser.Parse calls
// are commented out), so loadConfig always builds cfg from its hard-coded
// struct defaults regardless of os.Args -- exercising the clamp via os.Args
// would silently test nothing.
func TestClampExcessiveBlockSizeRespectsValueBelowWireCapacity(t *testing.T) {
	const belowCapacity uint64 = 64000000

	got := clampExcessiveBlockSize(belowCapacity)
	if got != belowCapacity {
		t.Fatalf("Expected ExcessiveBlockSize to be respected at %d, got %d", belowCapacity, got)
	}
}

// TestClampExcessiveBlockSizeCapsValueAboveWireCapacity confirms a configured
// (or defaulted) ExcessiveBlockSize above what the wire protocol can
// actually relay gets clamped down to that ceiling, instead of being
// advertised as an essentially-infinite value the transport can't back up.
func TestClampExcessiveBlockSizeCapsValueAboveWireCapacity(t *testing.T) {
	// Above the wire capacity ceiling.
	if got := clampExcessiveBlockSize(5000000000); got != maxWireMessagePayload {
		t.Fatalf("Expected ExcessiveBlockSize to be clamped to %d, got %d", maxWireMessagePayload, got)
	}

	// The old math.MaxUint64 default should also be clamped down.
	if got := clampExcessiveBlockSize(math.MaxUint64); got != maxWireMessagePayload {
		t.Fatalf("Expected default ExcessiveBlockSize to be clamped to %d, got %d", maxWireMessagePayload, got)
	}
}

// TestLoadConfigClampsDefaultExcessiveBlockSize confirms the clamp is wired
// up in loadConfig: since the CLI flag parser is disabled, cfg always starts
// from defaultExcessiveBlockSize (math.MaxUint64), which must come back
// clamped to the wire capacity ceiling.
func TestLoadConfigClampsDefaultExcessiveBlockSize(t *testing.T) {
	os.Args = []string{"bsvd"}

	cfg, _, err := loadConfig(ulogger.TestLogger{})
	if err != nil {
		t.Fatalf("Failed to load configuration: %v", err)
	}

	if cfg.ExcessiveBlockSize != maxWireMessagePayload {
		t.Fatalf("Expected default ExcessiveBlockSize to be clamped to %d, got %d", maxWireMessagePayload, cfg.ExcessiveBlockSize)
	}
}

func TestCreateDefaultConfigFile(t *testing.T) {
	// Setup a temporary directory
	tmpDir, err := ioutil.TempDir("", "bsvd")
	if err != nil {
		t.Fatalf("Failed creating a temporary directory: %v", err)
	}

	testpath := filepath.Join(tmpDir, "test.conf")

	// Clean-up
	defer func() {
		os.Remove(testpath)
		os.Remove(tmpDir)
	}()
	// err = createDefaultConfigFile(testpath)
	//
	//	if err != nil {
	//		t.Fatalf("Failed to create a default config file: %v", err)
	//	}
	//
	// content, err := ioutil.ReadFile(testpath)
	//
	//	if err != nil {
	//		t.Fatalf("Failed to read generated default config file: %v", err)
	//	}
}

func Test_setConfigValuesFromSettings(t *testing.T) {
	settings := map[string]string{
		"legacy_config_ShowVersion":             "true",              // bool
		"legacy_config_DataDir":                 "/tmp/test",         // string
		"legacy_config_AddPeers":                "peer1|peer2|peer3", // []string
		"legacy_config_MaxPeers":                "12",                // int
		"legacy_config_MinSyncPeerNetworkSpeed": "12345",             // uint64
		"legacy_config_BanDuration":             "23s",               // time.Duration (int64 natively)
		"legacy_config_BanThreshold":            "37",                // uint32
		"legacy_config_MinRelayTxFee":           "0.3",               // float64
		"legacy_config_SigCacheMaxSize":         "125",               // uint
	}
	testCfg := &config{}
	setConfigValuesFromSettings(ulogger.TestLogger{}, settings, testCfg)

	assert.True(t, testCfg.ShowVersion)
	assert.Equal(t, "/tmp/test", testCfg.DataDir)
	assert.Equal(t, []string{"peer1", "peer2", "peer3"}, testCfg.AddPeers)
	assert.Equal(t, 12, testCfg.MaxPeers)
	assert.Equal(t, uint64(12345), testCfg.MinSyncPeerNetworkSpeed)
	assert.Equal(t, 23*time.Second, testCfg.BanDuration)
	assert.Equal(t, uint32(37), testCfg.BanThreshold)
	assert.Equal(t, 0.3, testCfg.MinRelayTxFee)
	assert.Equal(t, uint(125), testCfg.SigCacheMaxSize)
}
