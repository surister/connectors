package sqlcapture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	cerrors "github.com/estuary/connectors/go/connector-errors"
	schemagen "github.com/estuary/connectors/go/schema-gen"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	pc "github.com/estuary/flow/go/protocols/capture"
	pf "github.com/estuary/flow/go/protocols/flow"
	log "github.com/sirupsen/logrus"
)

// Resource represents the capture configuration of a single table.
type Resource struct {
	Mode BackfillMode `json:"mode,omitempty" jsonschema:"title=Backfill Mode,description=How the preexisting contents of the table should be backfilled. This should generally not be changed.,default=,enum=,enum=Normal,enum=Precise,enum=Only Changes,enum=Without Primary Key"`

	Namespace string `json:"namespace" jsonschema:"title=Schema,description=The schema (namespace) in which the table resides.,readOnly=true"`
	Stream    string `json:"stream" jsonschema:"title=Table Name,description=The name of the table to be captured.,readOnly=true"`

	// PrimaryKey allows the user to override the "scan key" columns which will be used
	// to perform backfill queries and merge replicated changes. If left unset we default
	// to the collection's key, which is basically always what the user wants, so we omit
	// this property from the config schema to avoid confusion. However it is still supported,
	// if any captures set it.
	PrimaryKey []string `json:"primary_key,omitempty" jsonschema:"-"`

	DeprecatedSyncMode string `json:"syncMode,omitempty" jsonschema:"-"` // Unused, only supported to avoid breaking existing captures
}

// BackfillMode represents different ways we might want to backfill the preexisting contents of a table.
type BackfillMode string

const (
	// BackfillModeAutomatic means "use your best judgement at runtime".
	BackfillModeAutomatic = BackfillMode("")

	// BackfillModeNormal backfills chunks of the table and emits all
	// replication events regardless of whether they occur within the
	// backfilled portion of the table or not.
	BackfillModeNormal = BackfillMode("Normal")

	// BackfillModePrecise backfills chunks of the table and filters replication
	// events in portions of the table which haven't yet been reached.
	BackfillModePrecise = BackfillMode("Precise")

	// BackfillModeOnlyChanges skips backfilling the table entirely and jumps
	// directly to replication streaming for the entire dataset.
	BackfillModeOnlyChanges = BackfillMode("Only Changes")

	// BackfillModeWithoutKey can be used to capture tables without any form
	// of unique primary key, but lacks the exact correctness properties of
	// the normal backfill mode.
	BackfillModeWithoutKey = BackfillMode("Without Primary Key")
)

// Validate checks to make sure a resource appears usable.
func (r Resource) Validate() error {
	if !slices.Contains([]BackfillMode{BackfillModeAutomatic, BackfillModeNormal, BackfillModePrecise, BackfillModeOnlyChanges, BackfillModeWithoutKey}, r.Mode) {
		return fmt.Errorf("invalid backfill mode %q", r.Mode)
	}
	if r.Namespace == "" {
		return fmt.Errorf("table namespace unspecified")
	}
	if r.Stream == "" {
		return fmt.Errorf("table name unspecified")
	}
	return nil
}

// SetDefaults fills in the default values for unset optional parameters.
func (r *Resource) SetDefaults() {}

// Binding represents a capture binding.
type Binding struct {
	Index         uint32
	StreamID      string
	StateKey      boilerplate.StateKey
	Resource      Resource
	CollectionKey []string // JSON pointers
}

// Driver is an implementation of the pc.DriverServer interface which performs
// captures from a SQL database.
type Driver struct {
	ConfigSchema     json.RawMessage
	DocumentationURL string

	Connect func(ctx context.Context, name string, cfg json.RawMessage) (Database, error)
}

type prerequisitesError struct {
	errs []error
}

func (e *prerequisitesError) Error() string {
	var b = new(strings.Builder)
	fmt.Fprintf(b, "the capture cannot run due to the following error(s):")
	for _, err := range e.errs {
		b.WriteString("\n - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

func (e *prerequisitesError) Unwrap() []error {
	return e.errs
}

// docsURLFromEnv looks for an environment variable set as DOCS_URL to use for the spec response
// documentation URL. It uses that instead of the default documentation URL from the connector if
// found.
func docsURLFromEnv(providedURL string) string {
	fromEnv := os.Getenv("DOCS_URL")
	if fromEnv != "" {
		return fromEnv
	}

	return providedURL
}

// Spec returns the specification definition of this driver.
// Notably this includes its endpoint and resource configuration JSON schema.
func (d *Driver) Spec(ctx context.Context, req *pc.Request_Spec) (*pc.Response_Spec, error) {
	var resourceSchema, err = schemagen.GenerateSchema("SQL Database Resource Spec", &Resource{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating resource schema: %w", err)
	}
	return &pc.Response_Spec{
		Protocol:                 3032023,
		ConfigSchemaJson:         d.ConfigSchema,
		ResourceConfigSchemaJson: json.RawMessage(resourceSchema),
		DocumentationUrl:         docsURLFromEnv(d.DocumentationURL),
		ResourcePathPointers:     []string{"/namespace", "/stream"},
	}, nil
}

// Apply applies a new or updated capture to the store.
func (d *Driver) Apply(ctx context.Context, req *pc.Request_Apply) (*pc.Response_Applied, error) {
	// If there are no bindings in the request, then it may represent the deletion of the capture,
	// or it may also be that the all of the bindings were deleted but the capture remains.
	if len(req.Capture.Bindings) > 0 {
		var db, err = d.Connect(ctx, string(req.Capture.Name), req.Capture.ConfigJson)
		if err != nil {
			return nil, fmt.Errorf("error connecting to database: %w", err)
		}
		defer db.Close(ctx)

		discoveredTables, err := db.DiscoverTables(ctx)
		if err != nil {
			return nil, err
		}

		for _, binding := range req.Capture.Bindings {
			var res Resource
			if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
				return nil, fmt.Errorf("error parsing resource config: %w", err)
			}
			res.SetDefaults()

			var streamID = JoinStreamID(res.Namespace, res.Stream)

			if _, ok := discoveredTables[streamID]; !ok {
				return nil, fmt.Errorf("could not find or access table %q", streamID)
			}
		}
	}
	return &pc.Response_Applied{ActionDescription: ""}, nil
}

// Validate that store resources and proposed collection bindings are compatible.
func (d *Driver) Validate(ctx context.Context, req *pc.Request_Validate) (*pc.Response_Validated, error) {
	var db, err = d.Connect(ctx, string(req.Name), req.ConfigJson)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}
	defer db.Close(ctx)

	if _, err := DiscoverCatalog(ctx, db); err != nil {
		return nil, err
	}

	// Extract information about the previous state of various table bindings, so that we
	// can more easily compare to the incoming to-be-validated state.
	type previousBindingInfo struct {
		Resource Resource
		Backfill uint32
	}
	var previousBindings = make(map[StreamID]previousBindingInfo)
	if req.LastCapture != nil {
		for _, binding := range req.LastCapture.Bindings {
			var res Resource
			if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
				return nil, fmt.Errorf("error parsing resource config: %w", err)
			}
			res.SetDefaults()
			var streamID = JoinStreamID(res.Namespace, res.Stream)
			previousBindings[streamID] = previousBindingInfo{
				Resource: res,
				Backfill: binding.Backfill,
			}
		}
	}

	var errs = db.SetupPrerequisites(ctx)
	var out []*pc.Response_Validated_Binding
	for _, binding := range req.Bindings {
		var res Resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return nil, fmt.Errorf("error parsing resource config: %w", err)
		}
		res.SetDefaults()
		var streamID = JoinStreamID(res.Namespace, res.Stream)

		// When performing a keyed backfill, it's an error for the collection key to be the fallback key. It has to be one or more top-level properties.
		if (res.Mode == BackfillModeAutomatic || res.Mode == BackfillModeNormal || res.Mode == BackfillModePrecise) && len(res.PrimaryKey) == 0 {
			if slices.Equal(binding.Collection.Key, db.FallbackCollectionKey()) {
				errs = append(errs, fmt.Errorf("output collection for stream %q has the fallback key, which can't be used for a backfill", streamID))
			}
		}

		// If we have previous information about a resource and the backfill mode changes without
		// a corresponding backfill counter increment, that's an error.
		if prevBinding, ok := previousBindings[streamID]; ok && res.Mode != prevBinding.Resource.Mode && binding.Backfill <= prevBinding.Backfill {
			errs = append(errs, fmt.Errorf("must re-backfill when changing backfill mode: table %q changed from %q to %q", streamID, prevBinding.Resource.Mode, res.Mode))
		}

		if err := db.SetupTablePrerequisites(ctx, res.Namespace, res.Stream); err != nil {
			errs = append(errs, err)
			continue
		}

		out = append(out, &pc.Response_Validated_Binding{
			ResourcePath: []string{res.Namespace, res.Stream},
		})
	}
	if len(errs) > 0 {
		e := &prerequisitesError{errs}
		return nil, cerrors.NewUserError(nil, e.Error())
	}
	return &pc.Response_Validated{Bindings: out}, nil
}

// Discover returns the set of resources available from this Driver.
func (d *Driver) Discover(ctx context.Context, req *pc.Request_Discover) (*pc.Response_Discovered, error) {
	var db, err = d.Connect(ctx, "Flow Discovery", req.ConfigJson)
	if err != nil {
		return nil, fmt.Errorf("error connecting to database: %w", err)
	}
	defer db.Close(ctx)

	discoveredBindings, err := DiscoverCatalog(ctx, db)
	if err != nil {
		return nil, err
	}

	// Filter Flow materialization metadata tables out of the discovered catalog before output.
	// The materialization table names "flow_materializations_v2" and "flow_checkpoints_v1"
	// are expected to remain stable and may exist in any schema, depending on how the
	// materialization is configured.
	var filteredBindings = []*pc.Response_Discovered_Binding{} // Empty discovery must result in `[]` rather than `null`
	for _, binding := range discoveredBindings {
		var res Resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return nil, fmt.Errorf("error parsing resource config: %w", err)
		}
		res.SetDefaults()
		if res.Stream != "flow_materializations_v2" && res.Stream != "flow_checkpoints_v1" {
			filteredBindings = append(filteredBindings, binding)
		} else {
			log.WithFields(log.Fields{
				"schema": res.Namespace,
				"table":  res.Stream,
			}).Debug("filtered well-known table from discovery")
		}
	}

	return &pc.Response_Discovered{Bindings: filteredBindings}, nil
}

// Pull is a very long lived RPC through which the Flow runtime and a
// Driver cooperatively execute an unbounded number of transactions.
func (d *Driver) Pull(open *pc.Request_Open, stream *boilerplate.PullOutput) error {
	var state PersistentState
	if len(open.StateJson) > 0 {
		if err := pf.UnmarshalStrict(open.StateJson, &state); err != nil {
			return fmt.Errorf("unable to parse state checkpoint: %w", err)
		}
	}

	// This is a very hacky feature. Occasionally, connector bugs allow a capture task
	// to get into a bad state which would be easy to fix if we could issue some sort
	// of `flowctl secret edit-checkpoint <task>` command to manually edit the state
	// checkpoint, but which is very difficult to fix without such an ability.
	//
	// This bit of logic here allows us to simulate that sort of ability in extraordinary
	// cases, by adding an entry to this table of replacements and then doing a connector
	// release. The table is keyed by a hash of the task name and by the binlog cursor
	// value at which the replacement should occur, so the replacements are very narrowly
	// scoped.
	var hackyCursorReplacements = map[string]map[string]string{
		"TASKHASH415b69936fe1324600d67943e50235fc57fb7e0196b8f5f0TASKHASH": {`"binlog.000123:456789"`: `"binlog.000123:456789"`}, // Example
	}
	var hasher = sha256.New()
	hasher.Write([]byte(open.Capture.Name))
	var nameHash = fmt.Sprintf("%x", hasher.Sum(nil))
	log.WithFields(log.Fields{
		"namehash": nameHash,
		"cursor":   state.Cursor,
	}).Debug("checking for cursor replacements")
	if replacementsForTask, ok := hackyCursorReplacements[nameHash]; ok {
		if replacementForCursor, ok := replacementsForTask[string(state.Cursor)]; ok {
			log.WithFields(log.Fields{
				"namehash":  nameHash,
				"oldCursor": state.Cursor,
				"newCursor": replacementForCursor,
			}).Warn("cursor replacement triggered")
			state.Cursor = json.RawMessage(replacementForCursor)
		}
	}

	var ctx = stream.Context()
	log.WithField("eventType", "connectorStatus").Info("Connecting to database")
	db, err := d.Connect(ctx, string(open.Capture.Name), open.Capture.ConfigJson)
	if err != nil {
		return fmt.Errorf("error connecting to database: %w", err)
	}
	defer db.Close(ctx)

	log.WithField("eventType", "connectorStatus").Info("Verifying capture requirements")
	var errs = db.SetupPrerequisites(ctx)

	// Build a mapping from stream IDs to capture binding information
	var bindings = make(map[string]*Binding)
	for idx, binding := range open.Capture.Bindings {
		var res Resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return fmt.Errorf("error parsing resource config: %w", err)
		}
		res.SetDefaults()
		if err := db.SetupTablePrerequisites(ctx, res.Namespace, res.Stream); err != nil {
			errs = append(errs, err)
			continue
		}
		var streamID = JoinStreamID(res.Namespace, res.Stream)

		// TODO: Remove the whole 'Enable TxIDs' thing and instead include them unconditionally
		// at some point in the future once automatic schema updates are a thing and the
		// number of captures which would be broken by the change is acceptably small.
		for _, projection := range binding.Collection.Projections {
			if projection.Ptr == "/_meta/source/txid" {
				db.RequestTxIDs(res.Namespace, res.Stream)
			}
		}

		bindings[streamID] = &Binding{
			Index:         uint32(idx),
			StreamID:      streamID,
			StateKey:      boilerplate.StateKey(binding.StateKey),
			Resource:      res,
			CollectionKey: binding.Collection.Key,
		}
	}

	if len(errs) > 0 {
		e := &prerequisitesError{errs}
		return cerrors.NewUserError(nil, e.Error())
	}

	var c = Capture{
		Bindings: bindings,
		State:    &state,
		Output:   &boilerplate.PullOutput{Connector_CaptureServer: stream},
		Database: db,
	}

	// Notify Flow that we're ready and would like to receive acknowledgements.
	if err := c.Output.Ready(true); err != nil {
		return err
	}

	err = c.Run(ctx)
	if errors.Is(err, ErrFenceNotReached) {
		log.Warn("replication stream closed unexpectedly")
		return nil
	}
	return err
}
