package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/ordishs/gocore"
	"github.com/stretchr/testify/require"
)

// This file generalises the settings-doc drift guard originally written for
// docs/references/settings/services/blockvalidation_settings.md (a single,
// hand-picked file) to every file under docs/references/settings/, so a doc
// drifting away from the real default - or a configuration example using a
// key that does not exist - fails a test wherever it happens, not just in the
// one file that happened to get a guard.

// settingsDocsDir returns docs/references/settings relative to this test file.
func settingsDocsDir(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "unable to determine test file location")

	settingsDir := filepath.Dir(thisFile)
	repoRoot := filepath.Dir(settingsDir)

	return filepath.Join(repoRoot, "docs", "references", "settings")
}

// settingsDocFiles returns every markdown file under docs/references/settings,
// sorted for deterministic test output. Walking the directory (rather than
// hardcoding a list) means a new doc file is covered automatically.
func settingsDocFiles(t *testing.T) []string {
	t.Helper()

	dir := settingsDocsDir(t)

	var files []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}

		return nil
	})
	require.NoError(t, err, "walking %s", dir)
	require.NotEmpty(t, files, "no settings doc files found under %s", dir)

	sort.Strings(files)

	return files
}

// docKeyRe matches a bare settings key: letters, digits and underscores only.
// It is used to reject table cells that are prose ("Usage" text, etc.) rather
// than an actual environment variable key.
var docKeyRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// tableSeparatorCellRe matches one cell of a markdown table header separator
// row, e.g. the `---` or `:---:` in `|---|:---:|`.
var tableSeparatorCellRe = regexp.MustCompile(`^:?-+:?$`)

// docTableRow is one data row of a markdown settings table that documents a
// real settings key: the field name (for error messages), the documented
// default, and the environment variable key used to resolve the real default.
type docTableRow struct {
	setting    string
	docDefault string
	key        string
}

// splitTableRow splits a trimmed markdown table row ("| a | b | c |") into its
// cells, stripping the leading and trailing pipe first.
func splitTableRow(trimmedLine string) []string {
	inner := strings.TrimPrefix(trimmedLine, "|")
	inner = strings.TrimSuffix(inner, "|")

	cells := strings.Split(inner, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}

	return cells
}

// isTableSeparatorRow reports whether every cell of a table row is a header
// separator cell (only made of "-" and optional ":" anchors).
func isTableSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}

	for _, c := range cells {
		if !tableSeparatorCellRe.MatchString(c) {
			return false
		}
	}

	return true
}

// parseDocSettingsTables walks every markdown table in doc and extracts data
// rows from tables whose header has both a "Default" column and an
// "Environment Variable" column - the shape used by every settings-reference
// table in docs/references/settings. Tables documenting something else (URL
// query parameters, validation rules, enum-value tables, Kafka topic-name
// mappings without a default, etc.) do not have both columns and are skipped
// automatically; nothing needs to be hardcoded per file.
func parseDocSettingsTables(doc string) []docTableRow {
	var (
		rows                           []docTableRow
		inTable                        bool
		defaultIdx, envIdx, settingIdx int
	)

	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			continue
		}

		cells := splitTableRow(trimmed)

		if isTableSeparatorRow(cells) {
			continue
		}

		if !inTable {
			defaultIdx, envIdx, settingIdx = -1, -1, -1

			for i, c := range cells {
				switch c {
				case "Default":
					defaultIdx = i
				case "Environment Variable":
					envIdx = i
				case "Setting":
					settingIdx = i
				}
			}

			inTable = defaultIdx != -1 && envIdx != -1

			continue
		}

		if defaultIdx >= len(cells) || envIdx >= len(cells) {
			continue
		}

		key := cells[envIdx]
		if !docKeyRe.MatchString(key) {
			continue
		}

		row := docTableRow{docDefault: cells[defaultIdx], key: key}
		if settingIdx != -1 && settingIdx < len(cells) {
			row.setting = cells[settingIdx]
		}

		rows = append(rows, row)
	}

	return rows
}

// cpuHalfRe matches the ways the docs spell the max(4, NumCPU/2) default, which
// the struct tags spell as "auto".
var cpuHalfRe = regexp.MustCompile(`^max\(\s*4\s*,\s*CPU\s*/\s*2\s*\)$`)

// trailingParenRe matches a single, non-nested trailing parenthetical used to
// annotate a default, e.g. "0 (unlimited)", "false (true in settings.conf)",
// or `"localhost:8089" (Go default; overridden ... by settings.conf)`. Group 1
// is the value with the annotation removed; it is empty for a doc cell that is
// only an annotation, e.g. "(see below)".
var trailingParenRe = regexp.MustCompile(`^(.*?)\s*\([^()]*\)$`)

// stripOneQuoteLayer removes one layer of surrounding markdown code-span
// backticks, double quotes, or the angle brackets markdown wraps bare URLs in
// ("<http://...>"). It is applied in a loop because a doc cell can be quoted
// more than once, e.g. `"`http://host/path`"` (a backtick-quoted URL, itself
// wrapped in double quotes because it contains no spaces to protect).
func stripOneQuoteLayer(v string) string {
	switch {
	case len(v) >= 2 && strings.HasPrefix(v, "`") && strings.HasSuffix(v, "`"):
		return v[1 : len(v)-1]
	case len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`):
		return v[1 : len(v)-1]
	case len(v) >= 2 && strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">"):
		return v[1 : len(v)-1]
	default:
		return v
	}
}

// canonicalDefault normalises a documented or struct-tag default so the two
// can be compared: a trailing explanatory parenthetical is dropped, layered
// quoting (code spans, double quotes, markdown auto-link angle brackets) is
// stripped, the CPU-derived default is folded onto "auto", an explicit empty
// list/map ("[]"/"{}") is folded onto "" (the real default of every
// multi-string or string-set setting when unset), durations are parsed, and
// numbers are compared numerically.
func canonicalDefault(raw string) string {
	v := strings.TrimSpace(raw)

	// Checked before the trailing-parenthetical strip below: "max(4, CPU/2)" is
	// not a value with an explanatory annotation attached, it's the whole value.
	if cpuHalfRe.MatchString(v) {
		return "auto"
	}

	if m := trailingParenRe.FindStringSubmatch(v); m != nil {
		v = strings.TrimSpace(m[1])
	}

	for {
		stripped := stripOneQuoteLayer(v)
		if stripped == v {
			break
		}

		v = strings.TrimSpace(stripped)
	}

	if v == "[]" || v == "{}" || v == "map[]" {
		return ""
	}

	if cpuHalfRe.MatchString(v) {
		return "auto"
	}

	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}

	if d, err := time.ParseDuration(v); err == nil {
		// A duration field's zero value formats as "0s" (time.Duration.String()),
		// but a doc or struct tag documenting that same field's default often
		// just writes the bare "0" - both mean the same zero duration, so fold
		// them onto the same token rather than reporting a false mismatch.
		if d == 0 {
			return "0"
		}

		return d.String()
	}

	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}

	return v
}

// docDefaultsExemptions lists doc/key pairs that the three-way doc/tag/runtime
// comparison cannot fully verify, together with why. Each entry is checked to
// still exist in the doc so a fixed or removed row does not leave a stale
// exemption. Loosening canonicalDefault instead of listing these explicitly
// would let the guard silently stop checking rows it can't parse - worse than
// no guard, per the project rule this test is enforcing.
//
// unreliable names which anchor(s) this exemption says cannot be trusted for
// comparison against the other(s):
//   - "tag" - the struct tag is a static snapshot of a value actually computed
//     at runtime from another setting; doc/runtime is still checked and must
//     match, only the pairs involving the tag are skipped.
//   - "runtime" - the reverse: the value is only knowable at runtime (e.g. it
//     depends on the number of CPUs on the machine running the test), so the
//     doc and tag agree with each other on a formula/sentinel but neither can
//     be compared against the concrete number the runtime produces; doc/tag
//     is still checked and must match, only the pairs involving runtime are
//     skipped.
//   - "both" - neither anchor is trustworthy for this key (e.g. a key
//     resolution collision in ExportMetadata's flat map means both the tag
//     and the runtime value it reads back may reflect the wrong field); every
//     comparison for this row is skipped.
type docDefaultExemption struct {
	file       string // basename of the doc file
	key        string // environment variable key
	reason     string
	unreliable string // "tag", "runtime", or "both"
}

var docDefaultsExemptions = []docDefaultExemption{
	{
		file: "utxo_settings.md",
		key:  "utxostore_unminedTxRetention",
		reason: "the doc intentionally documents the runtime formula (globalBlockHeightRetention/2) rather than its " +
			"resolved number, so it cannot be text-compared against either anchor: the struct tag's default:\"144\" " +
			"is a static snapshot because tags cannot reference the runtime-computed globalBlockHeightRetention " +
			"variable, and the isolated-construction runtime value (144, matching the tag here) is only the " +
			"formula's result under globalBlockHeightRetention's own default - none of the three can be reduced to " +
			"the same comparable literal without a formula evaluator this test does not have",
		unreliable: "both",
	},
	{
		file: "utxo_settings.md",
		key:  "utxostore_parentPreservationBlocks",
		reason: "same as utxostore_unminedTxRetention above - the doc documents blocksInADayOnAverage*10 " +
			"symbolically, not its resolved number, so it cannot be text-compared against the tag's static " +
			"default:\"1440\" or the runtime's resolved 1440",
		unreliable: "both",
	},
	{
		file: "utxo_settings.md",
		key:  "utxostore_blockHeightRetention",
		reason: "same as utxostore_unminedTxRetention above - the doc documents globalBlockHeightRetention " +
			"symbolically, not its resolved number, so it cannot be text-compared against the tag's static " +
			"default:\"288\" or the runtime's resolved 288",
		unreliable: "both",
	},
	{
		file: "policy_settings.md",
		key:  "blockmaxsize",
		reason: "the key \"blockmaxsize\" is used by two different struct fields - PolicySettings.BlockMaxSize " +
			"(default 0, documented here, controls what this node mines) and BlockSettings.MaxSize (default " +
			"4294967296, controls what it accepts) - so ExportMetadata()'s flat key->default map holds whichever " +
			"one the reflection walk visits last, currently BlockSettings.MaxSize, for BOTH the tag default and the " +
			"runtime CurrentValue read back through the same map. This is a genuine settings.go ambiguity worth " +
			"fixing separately (surfaced here, not fixed, because resolving it changes runtime key-resolution " +
			"behaviour rather than a doc)",
		unreliable: "both",
	},
	{
		file: "blockvalidation_settings.md",
		key:  "blockvalidation_processTxMetaUsingStore_Concurrency",
		reason: "the doc (\"max(4, CPU/2)\") and the struct tag (default:\"auto\") agree with each other on the " +
			"formula, canonicalDefault folds both to \"auto\" - but the concrete runtime value is " +
			"max(4, runtime.NumCPU()/2) on whatever machine runs this test, which is not a fixed literal the doc " +
			"or tag can state and will vary by CI runner/laptop core count",
		unreliable: "runtime",
	},
	{
		file:       "blockvalidation_settings.md",
		key:        "blockvalidation_validateBlockSubtreesConcurrency",
		reason:     "same runtime.NumCPU()-derived default as blockvalidation_processTxMetaUsingStore_Concurrency above",
		unreliable: "runtime",
	},
	{
		file:       "blockvalidation_settings.md",
		key:        "blockvalidation_catchupConcurrency",
		reason:     "same runtime.NumCPU()-derived default as blockvalidation_processTxMetaUsingStore_Concurrency above",
		unreliable: "runtime",
	},
	{
		file:       "subtreevalidation_settings.md",
		key:        "subtreevalidation_getMissingTransactions",
		reason:     "same runtime.NumCPU()-derived default as blockvalidation_processTxMetaUsingStore_Concurrency above",
		unreliable: "runtime",
	},
}

// lookupExemption returns the exemption registered for file/key, if any.
func lookupExemption(file, key string) (docDefaultExemption, bool) {
	for _, e := range docDefaultsExemptions {
		if e.file == file && e.key == key {
			return e, true
		}
	}

	return docDefaultExemption{}, false
}

// isolatedSettingsContext is a gocore context name distinct from the ambient
// one (whatever SETTINGS_CONTEXT is set to, or gocore's own "dev" default -
// see gocore's Config()). Passing it to NewSettings makes
// gocore.Config(isolatedSettingsContext) create and cache its own copy of
// the confs map (see gocore's Config()) instead of returning the shared
// singleton every other test in this package reads through - so
// buildIsolatedRuntimeSettings below can empty that copy out without
// affecting anything else running in this test binary.
const isolatedSettingsContext = "settingsdoctest-isolated-context-3a1c9f"

// settingsKeys returns every settings-package key known to ExportMetadata().
// It only needs a throwaway Settings value: the key/tag list comes from
// reflection over the Settings type, not from any field's current value.
func settingsKeys(t *testing.T) []string {
	t.Helper()

	probe := &Settings{ChainCfgParams: &chaincfg.MainNetParams}

	keys := make([]string, 0, len(probe.ExportMetadata().Settings))
	for _, entry := range probe.ExportMetadata().Settings {
		keys = append(keys, entry.Key)
	}

	return keys
}

// buildIsolatedRuntimeSettings constructs a Settings the same way production
// code does - via NewSettings - but isolated from every source of config
// that could make the result depend on anything but the getX(...) literal
// fallback baked into settings.go:
//
//  1. settings.conf and settings_local.conf. The doc tables this test checks
//     document the getX(...) Go-literal fallback as "Default" and call out a
//     settings.conf override as a separate parenthetical when one exists
//     (see e.g. docs/references/settings/services/p2p_settings.md's
//     GRPCListenAddress row: "\":9906\" (Go default; overridden to `:9904`
//     by `settings.conf` ...)") - so "the value the settings package
//     actually produces" that this test must anchor to is the fallback, not
//     whatever settings.conf happens to ship, and settings.conf being
//     committed (unlike settings_local.conf) does not change that: it is a
//     deployment configuration file, not the thing the "Default" column
//     documents. Confirmed by running this uninstrumented: with settings.conf
//     applied, roughly 90 rows across every doc "mismatched" - overwhelmingly
//     tuned settings.conf values (batch sizes, ports, timeouts) that were
//     never doc/tag drift, which is what sent this investigation down the
//     right path.
//
//     gocore.Config(isolatedSettingsContext) (see gocore's Config()) starts
//     as a copy of the ambient singleton's confs map, which already has both
//     files merged in by the time any test in this package can observe it
//     (gocore.Config() loads them once, on the first call from anywhere in
//     the test binary - see gocore's Config()). Every key in that copy -
//     enumerated via GetAll() before any mutation - is Unset() one by one, so
//     the copy ends up with an empty confs map. This only touches the
//     isolatedSettingsContext copy; the shared "dev" (or whatever ambient
//     SETTINGS_CONTEXT) config every other test in this package relies on is
//     untouched.
//
//  2. OS environment variables. gocore's getInternal checks
//     os.LookupEnv(key) before consulting confs at all (see
//     (*gocore.Configuration).getInternal), so an ambient environment
//     variable named after a settings key would otherwise silently outrank
//     the code fallback regardless of step 1. Every variable whose name
//     matches a known settings key is unset for the duration of the call and
//     restored via t.Cleanup.
//
// A key that was never set is unaffected by either step: Unset on an absent
// confs key and Setenv/Unsetenv restore on a variable that was never set are
// both no-ops.
func buildIsolatedRuntimeSettings(t *testing.T, keys []string) *Settings {
	t.Helper()

	type envBackup struct {
		key    string
		value  string
		hadEnv bool
	}

	backups := make([]envBackup, 0, len(keys))

	for _, key := range keys {
		value, hadEnv := os.LookupEnv(key)
		backups = append(backups, envBackup{key: key, value: value, hadEnv: hadEnv})

		if hadEnv {
			require.NoError(t, os.Unsetenv(key), "unsetting ambient env var %s for isolation", key)
		}
	}

	t.Cleanup(func() {
		for _, b := range backups {
			if b.hadEnv {
				require.NoError(t, os.Setenv(b.key, b.value), "restoring ambient env var %s", b.key)
			}
		}
	})

	isolatedConfig := gocore.Config(isolatedSettingsContext)
	for key := range isolatedConfig.GetAll() {
		isolatedConfig.Unset(key)
	}

	return NewSettings(isolatedSettingsContext)
}

// TestSettingsDocDefaultsMatchCode walks every markdown file under
// docs/references/settings/ and cross-checks every settings-table row's
// documented default three ways: against the struct tag default surfaced by
// ExportMetadata(), and against the real value the settings package produces
// at runtime (NewSettings(), built in isolation - see
// buildIsolatedRuntimeSettings). A doc/tag-only check cannot catch the case
// where a doc and its struct tag drifted together: this PR found nine struct
// tags in settings/interface.go whose default:"..." no longer matched the
// literal passed to the corresponding getBool/getInt/getDuration(...) call in
// settings.go, in every case with the doc holding the correct value and the
// tag being the stale one - a doc-vs-tag comparison alone is blind to that
// class of bug because both sides being wrong the same way still compares
// equal.
func TestSettingsDocDefaultsMatchCode(t *testing.T) {
	tagProbe := &Settings{ChainCfgParams: &chaincfg.MainNetParams}

	codeDefaults := make(map[string]string)
	for _, entry := range tagProbe.ExportMetadata().Settings {
		codeDefaults[entry.Key] = entry.DefaultValue
	}

	runtimeSettings := buildIsolatedRuntimeSettings(t, settingsKeys(t))

	runtimeDefaults := make(map[string]string)
	for _, entry := range runtimeSettings.ExportMetadata().Settings {
		runtimeDefaults[entry.Key] = entry.CurrentValue
	}

	seenExemptions := make(map[docDefaultExemption]bool)

	for _, docPath := range settingsDocFiles(t) {
		docPath := docPath
		base := filepath.Base(docPath)

		t.Run(base, func(t *testing.T) {
			docBytes, err := os.ReadFile(docPath)
			require.NoError(t, err, "reading %s", docPath)

			rows := parseDocSettingsTables(string(docBytes))

			checked := 0
			var rowFailures []string

			for _, row := range rows {
				codeDefault, ok := codeDefaults[row.key]
				if !ok {
					continue
				}

				runtimeDefault, ok := runtimeDefaults[row.key]
				if !ok {
					continue
				}

				checked++

				exemption, exempt := lookupExemption(base, row.key)
				if exempt {
					seenExemptions[exemption] = true
				}

				skipTag := exempt && (exemption.unreliable == "tag" || exemption.unreliable == "both")
				skipRuntime := exempt && (exemption.unreliable == "runtime" || exemption.unreliable == "both")

				docC := canonicalDefault(row.docDefault)
				tagC := canonicalDefault(codeDefault)
				runtimeC := canonicalDefault(runtimeDefault)

				var mismatches []string

				if !skipTag && docC != tagC {
					mismatches = append(mismatches, "doc/tag")
				}

				if !skipRuntime && docC != runtimeC {
					mismatches = append(mismatches, "doc/runtime")
				}

				if !skipTag && !skipRuntime && tagC != runtimeC {
					mismatches = append(mismatches, "tag/runtime")
				}

				if len(mismatches) > 0 {
					rowFailures = append(rowFailures, fmt.Sprintf(
						"%s (%s): %v disagree - doc default %q, struct tag default %q, runtime default %q",
						row.setting, row.key, mismatches, row.docDefault, codeDefault, runtimeDefault))
				}
			}

			// Every row is checked before failing (rather than failing on the first
			// mismatch) so one test run reports every drifted row in this doc, not
			// just the first one found.
			require.Empty(t, rowFailures, "%s has %d mismatched row(s):\n%s",
				base, len(rowFailures), strings.Join(rowFailures, "\n"))

			minChecked, ok := minCheckedRowsPerDoc[base]
			require.True(t, ok, "no minimum-row expectation registered for %s - add one to minCheckedRowsPerDoc "+
				"(use 0 if this doc legitimately documents settings outside the standard table)", base)

			require.GreaterOrEqual(t, checked, minChecked,
				"only %d settings rows were cross-checked in %s (expected at least %d) - has the table format changed?",
				checked, base, minChecked)
		})
	}

	for _, e := range docDefaultsExemptions {
		require.True(t, seenExemptions[e],
			"stale exemption: %s/%s no longer appears as a settings-table row - remove it from docDefaultsExemptions", e.file, e.key)
	}
}

// minCheckedRowsPerDoc records, per doc file, the minimum number of
// settings-table rows that TestSettingsDocDefaultsMatchCode must be able to
// cross-check against ExportMetadata(). It exists so a doc reformat that
// silently stops the row parser from matching (as happened historically for
// the single file this guard used to cover) fails loudly per file instead of
// turning the test into a no-op. Values are set a little below the row count
// actually produced by the current table format, not to a nominal "1", so a
// large accidental drop in matched rows still fails even though a couple of
// rows moving around does not.
//
// One file is registered at 0 because it does not use the tabular
// "| Setting | Type | Default | Environment Variable | Usage |" format at
// all, so there is nothing for the table-row parser to check:
// blob_settings.md documents blob store URL query parameters (batch,
// sizeInBytes, ...), not settings-package keys - there is no ExportMetadata()
// entry to cross-check them against.
var minCheckedRowsPerDoc = map[string]int{
	"global_settings.md":            22,
	"kafka_settings.md":             17,
	"policy_settings.md":            24,
	"alert_settings.md":             5,
	"asset_settings.md":             32,
	"blockassembly_settings.md":     38,
	"blockchain_settings.md":        10,
	"blockpersister_settings.md":    8,
	"blockvalidation_settings.md":   60,
	"coinbase_settings.md":          19,
	"faucet_settings.md":            1,
	"legacy_settings.md":            21,
	"p2p_settings.md":               32,
	"propagation_settings.md":       12,
	"pruner_settings.md":            24,
	"rpc_settings.md":               10,
	"subtreevalidation_settings.md": 16,
	"utxopersister_settings.md":     2,
	"validator_settings.md":         16,
	"aerospike_settings.md":         12,
	"blob_settings.md":              0,
	"utxo_settings.md":              37,
}

// exampleKeyRe matches a `key = value` or `key=value` assignment at the start
// of a line inside the doc's configuration examples. The key may carry a
// settings-context suffix (e.g. `aerospike_host.docker.teranode1`); only the
// base key before the first "." is resolved against the settings package,
// since context suffixes are not part of ExportMetadata().
var exampleKeyRe = regexp.MustCompile(`(?m)^([A-Za-z][A-Za-z0-9_]*)((?:\.[A-Za-z0-9_]+)*)\s*=[^=]`)

// exampleKeyExemptions lists example-block tokens that look like a
// `key = value` assignment but are not settings-package keys, together with
// why. Each is checked to still appear in its doc.
type exampleKeyExemption struct {
	file   string
	key    string
	reason string
}

var exampleKeyExemptions = []exampleKeyExemption{
	{
		file: "global_settings.md", key: "SETTINGS_CONTEXT",
		reason: "bootstrap context selector read directly via gocore.Config().GetContext() before the settings " +
			"registry exists - it has no `key` struct tag and is not in ExportMetadata()",
	},
	{
		file: "pruner_settings.md", key: "startPruner",
		reason: "read directly via gocore.Config().GetBool(\"start\"+app) in daemon.go's shouldStart(), one per " +
			"service - it has no `key` struct tag and is not in ExportMetadata()",
	},
	{
		file: "pruner_settings.md", key: "PRUNER_GRPC_PORT",
		reason: "not a settings-package key at all - it's an operator-defined gocore config value referenced via " +
			"`${PRUNER_GRPC_PORT}` interpolation inside pruner_grpcAddress/pruner_grpcListenAddress (gocore's " +
			"generic ${VAR} substitution, see Configuration.replaceVariables), so it can be named anything",
	},
	{
		file: "pruner_settings.md", key: "PreserveUntil",
		reason: "a Go struct field assignment shown in a ```go code sample explaining the pruning calculation " +
			"(\"PreserveUntil = currentHeight + parentPreservationBlocks\"), not a settings.conf line",
	},
	{
		file: "pruner_settings.md", key: "pruner_IndexName",
		reason: "read directly via a package-level `var IndexName, _ = gocore.Config().Get(\"pruner_IndexName\", " +
			"\"pruner_dah_index\")` in stores/utxo/aerospike/pruner/pruner_service.go - it has no `key` struct " +
			"tag and is not in ExportMetadata()",
	},
}

// TestSettingsDocExampleKeysExist checks that every setting key used in a
// doc's configuration examples is a real key the settings package reads. A
// misspelled key in an example is silently ignored at runtime (gocore just
// never finds a match and falls back to the default), so the operator gets
// silent drift instead of an error.
func TestSettingsDocExampleKeysExist(t *testing.T) {
	s := &Settings{ChainCfgParams: &chaincfg.MainNetParams}

	knownKeys := make(map[string]struct{})
	for _, entry := range s.ExportMetadata().Settings {
		knownKeys[entry.Key] = struct{}{}
	}

	seenExemptions := make(map[exampleKeyExemption]bool)

	for _, docPath := range settingsDocFiles(t) {
		docPath := docPath
		base := filepath.Base(docPath)

		t.Run(base, func(t *testing.T) {
			docBytes, err := os.ReadFile(docPath)
			require.NoError(t, err, "reading %s", docPath)

			matches := exampleKeyRe.FindAllStringSubmatch(string(docBytes), -1)

			for _, match := range matches {
				key := match[1]

				isExempt := false
				for _, e := range exampleKeyExemptions {
					if e.file == base && e.key == key {
						seenExemptions[e] = true
						isExempt = true
					}
				}
				if isExempt {
					continue
				}

				_, ok := knownKeys[key]
				require.True(t, ok,
					"%s uses %q in a configuration example, but no such setting key exists", base, key)
			}
		})
	}

	for _, e := range exampleKeyExemptions {
		require.True(t, seenExemptions[e],
			"stale exemption: %s/%s no longer appears in a configuration example - remove it from exampleKeyExemptions", e.file, e.key)
	}
}
