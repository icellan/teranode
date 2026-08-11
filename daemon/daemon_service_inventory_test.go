package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// daemonServicesFile is the source file whose shouldStart() call sites define the
// authoritative set of services the daemon can start.
const daemonServicesFile = "daemon_services.go"

// daemonServiceNames maps the identifier of every "Formal" service-name constant
// that startServices() dispatches to that constant's value. The identifiers are
// cross-checked against the real shouldStart() call sites in daemonServicesFile, so
// this map cannot drift from the code in either direction.
var daemonServiceNames = map[string]string{
	"serviceAlertFormal":             serviceAlertFormal,
	"serviceAssetFormal":             serviceAssetFormal,
	"serviceBlockAssemblyFormal":     serviceBlockAssemblyFormal,
	"serviceBlockPersisterFormal":    serviceBlockPersisterFormal,
	"serviceBlockValidationFormal":   serviceBlockValidationFormal,
	"serviceBlockchainFormal":        serviceBlockchainFormal,
	"serviceLegacyFormal":            serviceLegacyFormal,
	"serviceNameP2PFormal":           serviceNameP2PFormal,
	"servicePropagationFormal":       servicePropagationFormal,
	"servicePrunerFormal":            servicePrunerFormal,
	"serviceRPCFormal":               serviceRPCFormal,
	"serviceSubtreeValidationFormal": serviceSubtreeValidationFormal,
	"serviceUtxoPersisterFormal":     serviceUtxoPersisterFormal,
	"serviceValidatorFormal":         serviceValidatorFormal,
}

// nonServiceShouldStartArgs holds shouldStart() arguments that are command-line
// switches rather than services, and so are exempt from the inventory.
var nonServiceShouldStartArgs = map[string]struct{}{
	// -help only prints usage; it starts nothing and has no services/ package.
	"serviceHelp": {},
}

// TestServiceInventory_MatchesServicesDirectory guards against documentation and
// code drift in both directions: the set of services startServices() can dispatch is
// derived from the actual shouldStart() call sites in daemon_services.go, so a
// service added to (or removed from) that function without being added to (or
// removed from) daemonServiceNames fails the test. Every inventoried service must
// also have a source package under services/<name>, which is what the architecture
// docs enumerate.
func TestServiceInventory_MatchesServicesDirectory(t *testing.T) {
	dispatched := shouldStartServiceIdents(t)

	// Direction 1: nothing the daemon dispatches may be missing from the inventory.
	for ident := range dispatched {
		_, listed := daemonServiceNames[ident]
		require.True(t, listed,
			"%s dispatches shouldStart(%s) but %s is missing from daemonServiceNames: add it here and add its services/<name> package",
			daemonServicesFile, ident, ident)
	}

	// Direction 2: nothing in the inventory may be a service the daemon no longer
	// dispatches.
	for ident := range daemonServiceNames {
		_, ok := dispatched[ident]
		require.True(t, ok,
			"daemonServiceNames lists %s but %s never passes it to shouldStart: remove it here or restore the dispatch",
			ident, daemonServicesFile)
	}

	// Every dispatched service must have a source package under services/<name>.
	servicesDir := filepath.Join("..", "services")

	for ident, name := range daemonServiceNames {
		dirName := strings.ToLower(name)
		pkgPath := filepath.Join(servicesDir, dirName)

		info, err := os.Stat(pkgPath)
		require.NoError(t, err, "daemon can start service %q (%s) but services/%s does not exist", name, ident, dirName)
		require.True(t, info.IsDir(), "services/%s exists but is not a directory", dirName)
	}
}

// shouldStartServiceIdents parses daemonServicesFile and returns the identifiers
// passed as the first argument to every d.shouldStart(...) call, minus the
// non-service switches. Deriving the set from the source rather than restating it
// keeps the guard bidirectional.
func shouldStartServiceIdents(t *testing.T) map[string]struct{} {
	t.Helper()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, daemonServicesFile, nil, 0)
	require.NoError(t, err, "failed to parse %s", daemonServicesFile)

	idents := make(map[string]struct{})

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "shouldStart" || len(call.Args) == 0 {
			return true
		}

		ident, ok := call.Args[0].(*ast.Ident)
		require.True(t, ok,
			"shouldStart is called with a non-constant service name at %s: this guard can only inventory constant identifiers",
			fset.Position(call.Lparen))

		if _, exempt := nonServiceShouldStartArgs[ident.Name]; exempt {
			return true
		}

		idents[ident.Name] = struct{}{}

		return true
	})

	require.NotEmpty(t, idents, "found no shouldStart call sites in %s: has the service dispatch moved?", daemonServicesFile)

	return idents
}
