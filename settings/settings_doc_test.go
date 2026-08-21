package settings

import (
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

	if v == "[]" || v == "{}" {
		return ""
	}

	if cpuHalfRe.MatchString(v) {
		return "auto"
	}

	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return strconv.FormatInt(n, 10)
	}

	if d, err := time.ParseDuration(v); err == nil {
		return d.String()
	}

	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}

	return v
}

// docDefaultsExemptions lists doc/key pairs that a straightforward text
// comparison cannot verify, together with why. Each entry is checked to still
// exist in the doc so a fixed or removed row does not leave a stale
// exemption. Loosening canonicalDefault instead of listing these explicitly
// would let the guard silently stop checking rows it can't parse - worse than
// no guard, per the project rule this test is enforcing.
type docDefaultExemption struct {
	file   string // basename of the doc file
	key    string // environment variable key
	reason string
}

var docDefaultsExemptions = []docDefaultExemption{
	{
		file: "utxo_settings.md",
		key:  "utxostore_unminedTxRetention",
		reason: "the real runtime default (settings.go) is globalBlockHeightRetention/2, matching the doc; the " +
			"struct tag's default:\"144\" is a static snapshot because tags cannot reference the runtime-computed " +
			"globalBlockHeightRetention variable - the doc is right, the tag is a documentation-only approximation",
	},
	{
		file: "utxo_settings.md",
		key:  "utxostore_parentPreservationBlocks",
		reason: "the real runtime default (settings.go) is blocksInADayOnAverage*10, matching the doc; the struct " +
			"tag's default:\"1440\" is a static snapshot for the same reason as utxostore_unminedTxRetention above",
	},
	{
		file: "utxo_settings.md",
		key:  "utxostore_blockHeightRetention",
		reason: "the real runtime default (settings.go) is globalBlockHeightRetention, matching the doc; the " +
			"struct tag's default:\"288\" is a static snapshot for the same reason as utxostore_unminedTxRetention above",
	},
	{
		file: "policy_settings.md",
		key:  "blockmaxsize",
		reason: "the key \"blockmaxsize\" is used by two different struct fields - PolicySettings.BlockMaxSize " +
			"(default 0, documented here, controls what this node mines) and BlockSettings.MaxSize (default " +
			"4294967296, controls what it accepts) - so ExportMetadata()'s flat key->default map holds whichever " +
			"one the reflection walk visits last, currently BlockSettings.MaxSize. This is a genuine settings.go " +
			"ambiguity worth fixing separately (surfaced here, not fixed, because resolving it changes runtime " +
			"key-resolution behaviour rather than a doc)",
	},
}

// isExemptDefault reports whether file/key is a documented exemption.
func isExemptDefault(file, key string) bool {
	for _, e := range docDefaultsExemptions {
		if e.file == file && e.key == key {
			return true
		}
	}

	return false
}

// TestSettingsDocDefaultsMatchCode walks every markdown file under
// docs/references/settings/ and cross-checks every settings-table row's
// documented default against the real default from the settings package (the
// struct tag default surfaced by ExportMetadata()). Two rows in the
// blockvalidation table were documented with each other's values for a long
// time while a guard existed for one hand-picked setting in one file - this
// covers every settings-table row in every doc file instead.
func TestSettingsDocDefaultsMatchCode(t *testing.T) {
	s := &Settings{ChainCfgParams: &chaincfg.MainNetParams}

	codeDefaults := make(map[string]string)
	for _, entry := range s.ExportMetadata().Settings {
		codeDefaults[entry.Key] = entry.DefaultValue
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

			for _, row := range rows {
				codeDefault, ok := codeDefaults[row.key]
				if !ok {
					continue
				}

				checked++

				for _, e := range docDefaultsExemptions {
					if e.file == base && e.key == row.key {
						seenExemptions[e] = true
					}
				}

				if isExemptDefault(base, row.key) {
					continue
				}

				require.Equal(t, canonicalDefault(codeDefault), canonicalDefault(row.docDefault),
					"%s documents %s (%s) with default %q, but the settings package defaults it to %q",
					base, row.setting, row.key, row.docDefault, codeDefault)
			}

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
// Two files are registered at 0 because they do not use the tabular
// "| Setting | Type | Default | Environment Variable | Usage |" format at
// all, so there is nothing for the table-row parser to check:
//   - blob_settings.md documents blob store URL query parameters (batch,
//     sizeInBytes, ...), not settings-package keys - there is no
//     ExportMetadata() entry to cross-check them against.
//   - pruner_settings.md documents settings via "### key" headings plus
//     "**Default**:"/"**Environment Variable**:" prose pairs instead of the
//     tabular format every other doc uses, so the table-row parser never
//     matches. This is a real coverage gap, not a false-positive dodge:
//     converting this doc to the standard table format would let it benefit
//     from this guard too.
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
	"pruner_settings.md":            0,
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
