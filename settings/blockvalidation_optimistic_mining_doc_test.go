package settings

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/stretchr/testify/require"
)

// TestOptimisticMiningDocMatchesCodeDefault guards against the settings reference
// doc drifting away from the code default for blockvalidation_optimistic_mining.
// It reads the documented default two ways - the settings table row in
// docs/references/settings/services/blockvalidation_settings.md, and the struct
// tag default surfaced via ExportMetadata() - and cross-checks both against the
// runtime default literal passed to getBool() in settings.go, so a change to
// either side without the other fails the test instead of silently drifting.
func TestOptimisticMiningDocMatchesCodeDefault(t *testing.T) {
	const settingKey = "blockvalidation_optimistic_mining"

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "unable to determine test file location")
	settingsDir := filepath.Dir(thisFile)
	repoRoot := filepath.Dir(settingsDir)

	// 1. Documented default from the settings table.
	docPath := filepath.Join(repoRoot, "docs", "references", "settings", "services", "blockvalidation_settings.md")
	docBytes, err := os.ReadFile(docPath)
	require.NoError(t, err, "reading settings reference doc")

	tableRowRe := regexp.MustCompile(`(?m)^\|\s*OptimisticMining\s*\|\s*bool\s*\|\s*(true|false)\s*\|\s*` + regexp.QuoteMeta(settingKey) + `\s*\|`)
	matches := tableRowRe.FindStringSubmatch(string(docBytes))
	require.Len(t, matches, 2, "could not find OptimisticMining settings table row in %s", docPath)
	docDefault := matches[1] == "true"

	// 2. Struct tag default, exposed via ExportMetadata() (settings/export.go).
	s := &Settings{ChainCfgParams: &chaincfg.MainNetParams}
	registry := s.ExportMetadata()

	var structTagDefault string
	found := false
	for _, entry := range registry.Settings {
		if entry.Key == settingKey {
			structTagDefault = entry.DefaultValue
			found = true
			break
		}
	}
	require.True(t, found, "setting %s not found in ExportMetadata()", settingKey)
	require.Contains(t, []string{"true", "false"}, structTagDefault,
		"struct tag default for %s is not a bool literal: %q", settingKey, structTagDefault)

	// 3. Runtime default literal passed to getBool() in settings.go.
	settingsGoPath := filepath.Join(settingsDir, "settings.go")
	settingsGoBytes, err := os.ReadFile(settingsGoPath)
	require.NoError(t, err, "reading settings.go")

	runtimeDefaultRe := regexp.MustCompile(`getBool\(\s*"` + regexp.QuoteMeta(settingKey) + `"\s*,\s*(true|false)\s*[,)]`)
	runtimeMatches := runtimeDefaultRe.FindStringSubmatch(string(settingsGoBytes))
	require.Len(t, runtimeMatches, 2, "could not find getBool default for %s in settings.go", settingKey)
	runtimeDefault := runtimeMatches[1] == "true"

	require.Equal(t, runtimeDefault, docDefault,
		"docs/references/settings/services/blockvalidation_settings.md documents OptimisticMining default as %v, "+
			"but settings.go defaults it to %v", docDefault, runtimeDefault)
	require.Equal(t, runtimeDefault, structTagDefault == "true",
		"struct tag default (%s) for %s disagrees with the settings.go runtime default (%v)",
		structTagDefault, settingKey, runtimeDefault)

	// 4. Stale-prose guard: these phrases described optimistic mining incorrectly (they named
	// script validation, rather than the block-level checks, as the deferred work) and were
	// corrected once already. If either the doc or the struct tag longdesc regresses back to one
	// of these phrases, fail even though the boolean default above still matches everywhere.
	stalePhrases := []string{
		"before full script validation completes",
		"Full script validation continues in parallel",
		"subtree validation runs in background",
	}

	blockvalidationSettingsGoPath := filepath.Join(settingsDir, "blockvalidation_settings.go")
	blockvalidationSettingsGoBytes, err := os.ReadFile(blockvalidationSettingsGoPath)
	require.NoError(t, err, "reading blockvalidation_settings.go")

	for _, phrase := range stalePhrases {
		require.NotContains(t, string(docBytes), phrase,
			"stale optimistic-mining prose is back in the settings doc: %q", phrase)
		require.NotContains(t, string(blockvalidationSettingsGoBytes), phrase,
			"stale optimistic-mining prose is back in the longdesc struct tag: %q", phrase)
	}
}
