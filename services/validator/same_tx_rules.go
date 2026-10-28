package validator

import (
	"sync"

	bdkscript "github.com/bitcoin-sv/bdk/module/gobdk/script"
	"github.com/bsv-blockchain/go-chaincfg"
	safeconversion "github.com/bsv-blockchain/go-safe-conversion"
)

// flagEngineKey identifies a BDK engine by everything that sets its flags: the
// network and the two activation heights teranode overrides.
type flagEngineKey struct {
	network   string
	genesis   uint32
	chronicle uint32
}

// flagEngines caches one BDK engine per flagEngineKey for SameTxRules.
var flagEngines sync.Map

// SameTxRules reports whether a transaction valid at height a is checked under the
// same consensus rules at height b: BDK computes the same consensus script flags
// at both, and no BIP68 activation (ChainCfgParams.CSVHeight) lies between them.
// The flags come from BDK itself rather than a list of chaincfg heights: BDK
// enables P2SH at a height chaincfg does not carry, and UAHF and DAA one block
// after their fork heights. A network BDK does not know returns false.
func SameTxRules(params *chaincfg.Params, a, b uint32) bool {
	lo, hi := min(a, b), max(a, b)
	if csv := params.CSVHeight; csv > lo && csv <= hi {
		return false
	}

	se := flagEngine(params)
	if se == nil {
		return false
	}

	loH, err := safeconversion.Uint32ToInt32(lo)
	if err != nil {
		return false
	}

	hiH, err := safeconversion.Uint32ToInt32(hi)
	if err != nil {
		return false
	}

	// Two UTXO heights, pre- and post-Genesis: the per-input flags depend on them.
	for _, utxoHeight := range []int32{0, loH} {
		if se.CalculateFlags(utxoHeight, loH, true) != se.CalculateFlags(utxoHeight, hiH, true) {
			return false
		}
	}

	return true
}

// flagEngine returns the cached BDK engine for params, creating it on first use.
func flagEngine(params *chaincfg.Params) *bdkscript.TxValidator {
	key := flagEngineKey{
		network:   getBDKChainNameFromParams(nil, params),
		genesis:   params.GenesisActivationHeight,
		chronicle: params.ChronicleActivationHeight,
	}

	if se, ok := flagEngines.Load(key); ok {
		return se.(*bdkscript.TxValidator)
	}

	if key.network == "" {
		return nil
	}

	se := bdkscript.NewTxValidator(key.network)
	if se == nil {
		return nil
	}

	genesis, err := safeconversion.Uint32ToInt32(key.genesis)
	if err != nil || se.SetGenesisActivationHeight(genesis) != nil {
		return nil
	}

	chronicle, err := safeconversion.Uint32ToInt32(key.chronicle)
	if err != nil || se.SetChronicleActivationHeight(chronicle) != nil {
		return nil
	}

	actual, _ := flagEngines.LoadOrStore(key, se)

	return actual.(*bdkscript.TxValidator)
}
