package boilerplate

import (
	"context"
	"embed"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bradleyjkemp/cupaloy"
	pf "github.com/estuary/flow/go/protocols/flow"
	pm "github.com/estuary/flow/go/protocols/materialize"
	"github.com/stretchr/testify/require"
)

//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/add-and-remove-many.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/add-single-optional.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/base.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/big-schema-changed.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/big-schema-nullable.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/big-schema.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/challenging-fields.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/remove-single-optional.flow.yaml
//go:generate ./testdata/generate-spec-proto.sh testdata/validate_apply_test_cases/remove-single-required.flow.yaml

//go:embed testdata/validate_apply_test_cases/generated_specs
var applyValidateFs embed.FS

func loadSpec(t *testing.T, path string) *pf.MaterializationSpec {
	t.Helper()

	specBytes, err := applyValidateFs.ReadFile(filepath.Join("testdata/validate_apply_test_cases/generated_specs", path))
	require.NoError(t, err)
	var spec pf.MaterializationSpec
	require.NoError(t, spec.Unmarshal(specBytes))

	return &spec
}

func RunValidateAndApplyTestCases(
	t *testing.T,
	driver Connector,
	config any,
	resourceConfig any,
	dumpSchema func(t *testing.T) string,
	cleanup func(t *testing.T),
) {
	ctx := context.Background()
	var snap strings.Builder

	configJson, err := json.Marshal(config)
	require.NoError(t, err)

	resourceConfigJson, err := json.Marshal(resourceConfig)
	require.NoError(t, err)

	t.Run("validate and apply many different types of fields", func(t *testing.T) {
		defer cleanup(t)

		fixture := loadSpec(t, "big-schema.flow.proto")

		// Initial validation with no previously existing table.
		validateRes, err := driver.Validate(ctx, ValidateReq(fixture, nil, configJson, resourceConfigJson))
		require.NoError(t, err)

		snap.WriteString("Big Schema Initial Constraints:\n")
		snap.WriteString(SnapshotConstraints(t, validateRes.Bindings[0].Constraints))

		// Initial apply with no previously existing table.
		_, err = driver.Apply(ctx, ApplyReq(fixture, nil, configJson, resourceConfigJson, validateRes, true))
		require.NoError(t, err)

		sch := dumpSchema(t)

		// Validate again.
		validateRes, err = driver.Validate(ctx, ValidateReq(fixture, fixture, configJson, resourceConfigJson))
		require.NoError(t, err)

		snap.WriteString("\nBig Schema Re-validated Constraints:\n")
		snap.WriteString(SnapshotConstraints(t, validateRes.Bindings[0].Constraints))

		// Apply again - this should be a no-op.
		_, err = driver.Apply(ctx, ApplyReq(fixture, fixture, configJson, resourceConfigJson, validateRes, true))
		require.NoError(t, err)
		require.Equal(t, sch, dumpSchema(t))

		// Validate with most of the field types changed somewhat randomly.
		changed := loadSpec(t, "big-schema-changed.flow.proto")
		validateRes, err = driver.Validate(ctx, ValidateReq(changed, fixture, configJson, resourceConfigJson))
		require.NoError(t, err)

		snap.WriteString("\nBig Schema Changed Types Constraints:\n")
		snap.WriteString(SnapshotConstraints(t, validateRes.Bindings[0].Constraints))

		snap.WriteString("\nBig Schema Materialized Resource Schema With All Fields Required:\n")
		snap.WriteString(sch)

		// Validate and apply the schema with all fields removed from required and snapshot the
		// table output.
		nullable := loadSpec(t, "big-schema-nullable.flow.proto")
		validateRes, err = driver.Validate(ctx, ValidateReq(nullable, fixture, configJson, resourceConfigJson))
		require.NoError(t, err)

		_, err = driver.Apply(ctx, ApplyReq(nullable, fixture, configJson, resourceConfigJson, validateRes, true))
		require.NoError(t, err)

		// A second apply of the nullable schema should be a no-op.
		sch = dumpSchema(t)
		_, err = driver.Apply(ctx, ApplyReq(nullable, nullable, configJson, resourceConfigJson, validateRes, true))
		require.NoError(t, err)
		require.Equal(t, sch, dumpSchema(t))

		snap.WriteString("\nBig Schema Materialized Resource Schema With No Fields Required:\n")
		snap.WriteString(sch)

		// Apply the spec with the randomly changed types, but this time with a backfill that will
		// cause the table to be replaced.
		changed.Bindings[0].Backfill = 1
		validateRes, err = driver.Validate(ctx, ValidateReq(changed, nullable, configJson, resourceConfigJson))
		require.NoError(t, err)

		snap.WriteString("\nBig Schema Changed Types With Table Replacement Constraints:\n")
		snap.WriteString(SnapshotConstraints(t, validateRes.Bindings[0].Constraints))

		_, err = driver.Apply(ctx, ApplyReq(changed, nullable, configJson, resourceConfigJson, validateRes, true))
		require.NoError(t, err)
		snap.WriteString("\nBig Schema Materialized Resource Schema Changed Types With Table Replacement:\n")
		snap.WriteString(dumpSchema(t) + "\n")
	})

	t.Run("add and remove fields", func(t *testing.T) {
		tests := []struct {
			name    string
			newSpec *pf.MaterializationSpec
		}{
			{
				name:    "add a single field",
				newSpec: loadSpec(t, "add-single-optional.flow.proto"),
			},
			{
				name:    "remove a single optional field",
				newSpec: loadSpec(t, "remove-single-optional.flow.proto"),
			},
			{
				name:    "remove a single required field",
				newSpec: loadSpec(t, "remove-single-required.flow.proto"),
			},
			{
				name:    "add and remove many fields",
				newSpec: loadSpec(t, "add-and-remove-many.flow.proto"),
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				defer cleanup(t)

				initial := loadSpec(t, "base.flow.proto")

				// Validate and Apply the base spec.
				validateRes, err := driver.Validate(ctx, ValidateReq(initial, nil, configJson, resourceConfigJson))
				require.NoError(t, err)
				_, err = driver.Apply(ctx, ApplyReq(initial, nil, configJson, resourceConfigJson, validateRes, true))
				require.NoError(t, err)

				// Validate and Apply the updated spec.
				validateRes, err = driver.Validate(ctx, ValidateReq(tt.newSpec, initial, configJson, resourceConfigJson))
				require.NoError(t, err)
				_, err = driver.Apply(ctx, ApplyReq(tt.newSpec, initial, configJson, resourceConfigJson, validateRes, true))
				require.NoError(t, err)

				snap.WriteString(tt.name + ":\n")
				snap.WriteString(string(dumpSchema(t)) + "\n")
			})
		}
	})

	t.Run("validate and apply fields with challenging names", func(t *testing.T) {
		defer cleanup(t)

		fixture := loadSpec(t, "challenging-fields.flow.proto")

		// Validate and apply twice to make sure that a re-application does not attempt to re-create
		// any columns. This makes sure we are able to read back the schema we have created
		// correctly.
		for idx := 0; idx < 2; idx++ {
			validateRes, err := driver.Validate(ctx, ValidateReq(fixture, nil, configJson, resourceConfigJson))
			require.NoError(t, err)
			_, err = driver.Apply(ctx, ApplyReq(fixture, nil, configJson, resourceConfigJson, validateRes, false))
			require.NoError(t, err)
		}

		snap.WriteString("Challenging Field Names Materialized Columns:\n")
		snap.WriteString(dumpSchema(t))
	})

	cupaloy.SnapshotT(t, snap.String())
}

// validateReq makes a mock Validate request object from a built spec fixture. It only works with a
// single binding.
func ValidateReq(spec *pf.MaterializationSpec, lastSpec *pf.MaterializationSpec, config json.RawMessage, resourceConfig json.RawMessage) *pm.Request_Validate {
	req := &pm.Request_Validate{
		Name:          spec.Name,
		ConnectorType: spec.ConnectorType,
		ConfigJson:    config,
		Bindings: []*pm.Request_Validate_Binding{{
			ResourceConfigJson: resourceConfig,
			Collection:         spec.Bindings[0].Collection,
			FieldConfigJsonMap: spec.Bindings[0].FieldSelection.FieldConfigJsonMap,
			Backfill:           spec.Bindings[0].Backfill,
		}},
		LastMaterialization: lastSpec,
	}

	return req
}

// applyReq conjures a pm.Request_Apply from a spec and validate response.
func ApplyReq(spec *pf.MaterializationSpec, lastSpec *pf.MaterializationSpec, config json.RawMessage, resourceConfig json.RawMessage, validateRes *pm.Response_Validated, includeOptional bool) *pm.Request_Apply {
	spec.ConfigJson = config
	spec.Bindings[0].ResourceConfigJson = resourceConfig
	spec.Bindings[0].ResourcePath = validateRes.Bindings[0].ResourcePath
	spec.Bindings[0].DeltaUpdates = validateRes.Bindings[0].DeltaUpdates
	spec.Bindings[0].FieldSelection = SelectedFields(validateRes.Bindings[0], spec.Bindings[0].Collection, includeOptional)

	req := &pm.Request_Apply{
		Materialization:     spec,
		Version:             "someVersion",
		LastMaterialization: lastSpec,
	}

	return req
}

// selectedFields creates a field selection that includes all possible fields.
func SelectedFields(binding *pm.Response_Validated_Binding, collection pf.CollectionSpec, includeOptional bool) pf.FieldSelection {
	out := pf.FieldSelection{}

	for field, constraint := range binding.Constraints {
		if constraint.Type.IsForbidden() || !includeOptional && constraint.Type == pm.Response_Validated_Constraint_FIELD_OPTIONAL {
			continue
		}

		proj := collection.GetProjection(field)
		if proj.IsPrimaryKey {
			out.Keys = append(out.Keys, field)
		} else if proj.IsRootDocumentProjection() {
			if out.Document == "" {
				out.Document = field
			} else {
				// Handle cases with more than one root document projection selected - the "first"
				// one is the document, and the rest are materialized as values.
				out.Values = append(out.Values, field)
			}
		} else {
			out.Values = append(out.Values, field)
		}
	}

	slices.Sort(out.Keys)
	slices.Sort(out.Values)

	return out
}

// snapshotConstraints makes a compact string representation of a set of constraints, with one
// constraint printed per line.
func SnapshotConstraints(t *testing.T, cs map[string]*pm.Response_Validated_Constraint) string {
	t.Helper()

	type constraintRow struct {
		Field      string
		Type       int
		TypeString string
		Reason     string
	}

	rows := make([]constraintRow, 0, len(cs))
	for f, c := range cs {
		rows = append(rows, constraintRow{
			Field:      f,
			Type:       int(c.Type),
			TypeString: c.Type.String(),
			Reason:     c.Reason,
		})
	}

	slices.SortFunc(rows, func(i, j constraintRow) int {
		return strings.Compare(i.Field, j.Field)
	})

	var out strings.Builder
	enc := json.NewEncoder(&out)
	for _, r := range rows {
		require.NoError(t, enc.Encode(r))
	}

	return out.String()
}
