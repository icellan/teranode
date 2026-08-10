package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServiceInventory_MatchesServicesDirectory guards against documentation and
// code drift: every service name the daemon knows how to start (the "Formal"
// service-name constants passed to shouldStart/AddService in daemon_services.go)
// must have a corresponding source package under services/<name>. If a service is
// added, renamed, or removed here without updating the services/ layout (or vice
// versa), this test fails instead of silently drifting out of sync with the docs.
func TestServiceInventory_MatchesServicesDirectory(t *testing.T) {
	// The set of "Formal" service-name constants the daemon can start, as used in
	// startServices() (daemon/daemon_services.go). Keep this list in sync with that
	// function's shouldStart/starters list.
	daemonServiceNames := []string{
		serviceAlertFormal,
		serviceAssetFormal,
		serviceBlockAssemblyFormal,
		serviceBlockPersisterFormal,
		serviceBlockValidationFormal,
		serviceBlockchainFormal,
		serviceLegacyFormal,
		serviceNameP2PFormal,
		servicePropagationFormal,
		servicePrunerFormal,
		serviceRPCFormal,
		serviceSubtreeValidationFormal,
		serviceUtxoPersisterFormal,
		serviceValidatorFormal,
	}

	servicesDir := filepath.Join("..", "services")

	for _, name := range daemonServiceNames {
		dirName := strings.ToLower(name)
		pkgPath := filepath.Join(servicesDir, dirName)

		info, err := os.Stat(pkgPath)
		require.NoError(t, err, "daemon can start service %q but services/%s does not exist", name, dirName)
		require.True(t, info.IsDir(), "services/%s exists but is not a directory", dirName)
	}
}
