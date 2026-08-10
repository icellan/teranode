package settings

import (
	"reflect"
	"testing"

	"github.com/bsv-blockchain/go-chaincfg"
	"github.com/stretchr/testify/require"
)

// TestMetadataCoverage verifies that all expected settings are discovered by reflection.
func TestMetadataCoverage(t *testing.T) {
	settings := &Settings{
		ChainCfgParams: &chaincfg.MainNetParams,
	}

	registry := settings.ExportMetadata()

	// We should have 398 settings based on the complete migration
	require.NotNil(t, registry)
	require.GreaterOrEqual(t, len(registry.Settings), 300, "Should have at least 300 settings after complete migration")

	// Verify all categories are present
	require.NotEmpty(t, registry.Categories)
	require.Contains(t, registry.Categories, CategoryGlobal)
	require.Contains(t, registry.Categories, CategoryKafka)
	require.Contains(t, registry.Categories, CategoryAerospike)
	require.Contains(t, registry.Categories, CategoryP2P)
	require.Contains(t, registry.Categories, CategoryBlockAssembly)
	require.Contains(t, registry.Categories, CategoryBlockValidation)

	// Create a map for validation
	categoryCounts := make(map[string]int)
	for _, setting := range registry.Settings {
		categoryCounts[setting.Category]++

		// Verify each setting has required fields
		require.NotEmpty(t, setting.Key, "Setting must have a key")
		require.NotEmpty(t, setting.Name, "Setting must have a name")
		require.NotEmpty(t, setting.Type, "Setting must have a type")
		require.NotEmpty(t, setting.Category, "Setting must have a category")
		require.NotEmpty(t, setting.Description, "Setting must have a description")
		// DefaultValue and CurrentValue can be empty for some settings
	}

	// Verify major categories have settings
	require.Greater(t, categoryCounts[CategoryGlobal], 10, "Global should have many settings")
	require.Greater(t, categoryCounts[CategoryKafka], 20, "Kafka should have 20+ settings")
	require.Greater(t, categoryCounts[CategoryP2P], 15, "P2P should have 15+ settings")
	require.Greater(t, categoryCounts[CategoryBlockAssembly], 15, "BlockAssembly should have 15+ settings")
	require.Greater(t, categoryCounts[CategoryBlockValidation], 20, "BlockValidation should have 20+ settings")
	require.Greater(t, categoryCounts[CategoryUtxoStore], 20, "UtxoStore should have 20+ settings")

	t.Logf("Total settings discovered: %d", len(registry.Settings))
	t.Logf("Category distribution: %+v", categoryCounts)
}

// noTagExemptFields lists Settings fields (dotted path from the Settings
// root, e.g. "Kafka.BlocksValidate") that intentionally carry no "key" tag
// and are therefore not exported via ExportMetadata:
//   - Commit, Version, Context, IsAllInOneMode are populated at runtime
//     (build info, config context, process topology), not read from
//     configuration.
//   - Kafka.BlocksValidate is an unused internal field (see its own
//     "not directly configured" comment in kafka_settings.go).
var noTagExemptFields = map[string]bool{
	"Commit":               true,
	"Version":              true,
	"Context":              true,
	"IsAllInOneMode":       true,
	"Kafka.BlocksValidate": true,
}

// TestNoMissingTags walks the Settings struct via reflection and fails if any
// leaf (non-struct) field lacks the "key" struct tag that extractFields (see
// export.go) relies on to expose settings via ExportMetadata. Nested structs
// declared within the settings package are walked recursively; structs from
// other packages (e.g. *chaincfg.Params) are opaque and left unchecked, since
// they are not ours to tag. Fields listed in noTagExemptFields are skipped.
func TestNoMissingTags(t *testing.T) {
	settingsPkgPath := reflect.TypeOf(Settings{}).PkgPath()

	var missing []string

	var walk func(typ reflect.Type, prefix string)
	walk = func(typ reflect.Type, prefix string) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			fieldName := prefix + field.Name

			if field.Tag.Get("key") != "" {
				continue
			}

			if noTagExemptFields[fieldName] {
				continue
			}

			fieldType := field.Type
			if fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}

			if fieldType.Kind() == reflect.Struct {
				// Structs declared outside the settings package (e.g.
				// *chaincfg.Params) are opaque - they are not ours to tag,
				// so neither recurse into them nor flag them as missing.
				if fieldType.PkgPath() != settingsPkgPath {
					continue
				}

				walk(fieldType, fieldName+".")
				continue
			}

			missing = append(missing, fieldName)
		}
	}

	walk(reflect.TypeOf(Settings{}), "")

	require.Empty(t, missing, `settings field(s) missing a "key" struct tag (add one, or add the field to noTagExemptFields if it is genuinely exempt)`)
}
