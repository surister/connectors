package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/estuary/connectors/go/encrow"
	"github.com/estuary/connectors/go/schedule"
	schemagen "github.com/estuary/connectors/go/schema-gen"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	pc "github.com/estuary/flow/go/protocols/capture"
	pf "github.com/estuary/flow/go/protocols/flow"
	"github.com/invopop/jsonschema"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// BatchSQLDriver represents a generic "batch SQL" capture behavior, parameterized
// by a config schema, connect function, and value translation logic.
type BatchSQLDriver struct {
	DocumentationURL string
	ConfigSchema     json.RawMessage

	Connect              func(ctx context.Context, cfg *Config) (*sql.DB, error)
	TranslateValue       func(val any, databaseTypeName string) (any, error)
	GenerateResource     func(resourceName, schemaName, tableName, tableType string) (*Resource, error)
	DefaultQueryTemplate string
}

// Resource represents the capture configuration of a single resource binding.
type Resource struct {
	Name string `json:"name" jsonschema:"title=Resource Name,description=The unique name of this resource." jsonschema_extras:"order=0"`

	SchemaName string   `json:"schema,omitempty" jsonschema:"title=Schema Name,description=The name of the schema in which the captured table lives. The query template must be overridden if this is unset."  jsonschema_extras:"order=1"`
	TableName  string   `json:"table,omitempty" jsonschema:"title=Table Name,description=The name of the table to be captured. The query template must be overridden if this is unset."  jsonschema_extras:"order=2"`
	Cursor     []string `json:"cursor,omitempty" jsonschema:"title=Cursor Columns,description=The names of columns which should be persisted between query executions as a cursor." jsonschema_extras:"order=3"`

	PollSchedule string `json:"poll,omitempty" jsonschema:"title=Polling Schedule,description=When and how often to execute the fetch query (overrides the connector default setting). Accepts a Go duration string like '5m' or '6h' for frequency-based polling or a string like 'daily at 12:34Z' to poll at a specific time (specified in UTC) every day." jsonschema_extras:"order=4,pattern=^([-+]?([0-9]+([.][0-9]+)?(h|m|s|ms))+|daily at [0-9][0-9]?:[0-9]{2}Z)$"`
	Template     string `json:"template,omitempty" jsonschema:"title=Query Template Override,description=Optionally overrides the query template which will be rendered and then executed. Consult documentation for examples." jsonschema_extras:"multiline=true,order=5"`
}

// Validate checks that the resource spec possesses all required properties.
func (r Resource) Validate() error {
	var requiredProperties = [][]string{
		{"name", r.Name},
	}
	for _, req := range requiredProperties {
		if req[1] == "" {
			return fmt.Errorf("missing '%s'", req[0])
		}
	}
	if r.Template == "" && (r.SchemaName == "" || r.TableName == "") {
		return fmt.Errorf("must specify schema+table name or else a template override")
	}
	if r.Template != "" {
		if _, err := template.New("query").Funcs(templateFuncs).Parse(r.Template); err != nil {
			return fmt.Errorf("error parsing template: %w", err)
		}
	}
	if slices.Contains(r.Cursor, "") {
		return fmt.Errorf("cursor column names can't be empty (got %q)", r.Cursor)
	}
	if r.PollSchedule != "" {
		if err := schedule.Validate(r.PollSchedule); err != nil {
			return fmt.Errorf("invalid polling schedule %q: %w", r.PollSchedule, err)
		}
	}
	return nil
}

// documentMetadata contains the source metadata located at /_meta
type documentMetadata struct {
	Polled time.Time `json:"polled" jsonschema:"title=Polled Timestamp,description=The time at which the update query which produced this document as executed."`
	Index  int       `json:"index" jsonschema:"title=Result Index,description=The index of this document within the query execution which produced it."`
}

// The fallback collection key just refers to the polling iteration and result index of each document.
var fallbackKey = []string{"/_meta/polled", "/_meta/index"}

func generateCollectionSchema(keyColumns []string, columnTypes map[string]*jsonschema.Schema) (json.RawMessage, error) {
	// Generate schema for the metadata via reflection
	var reflector = jsonschema.Reflector{
		ExpandedStruct: true,
		DoNotReference: true,
	}
	var metadataSchema = reflector.ReflectFromType(reflect.TypeOf(documentMetadata{}))
	metadataSchema.Definitions = nil
	metadataSchema.AdditionalProperties = nil

	var required = []string{"_meta"}
	var properties = map[string]*jsonschema.Schema{
		"_meta": metadataSchema,
	}

	for _, colName := range keyColumns {
		var columnType = columnTypes[colName]
		if columnType == nil {
			return nil, fmt.Errorf("unable to add key column %q to schema: type unknown", colName)
		}
		properties[colName] = columnType
		required = append(required, colName)
	}

	var schema = &jsonschema.Schema{
		Type:                 "object",
		Required:             required,
		AdditionalProperties: nil,
		Extras: map[string]interface{}{
			"properties":     properties,
			"x-infer-schema": true,
		},
	}

	// Marshal schema to JSON
	bs, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("error serializing schema: %w", err)
	}
	return json.RawMessage(bs), nil
}

var minimalSchema = func() json.RawMessage {
	var schema, err = generateCollectionSchema(nil, nil)
	if err != nil {
		panic(err)
	}
	return schema
}()

// Spec returns metadata about the capture connector.
func (drv *BatchSQLDriver) Spec(ctx context.Context, req *pc.Request_Spec) (*pc.Response_Spec, error) {
	resourceSchema, err := schemagen.GenerateSchema("Batch SQL Resource Spec", &Resource{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating resource schema: %w", err)
	}

	return &pc.Response_Spec{
		ConfigSchemaJson:         drv.ConfigSchema,
		ResourceConfigSchemaJson: resourceSchema,
		DocumentationUrl:         drv.DocumentationURL,
		ResourcePathPointers:     []string{"/name"},
	}, nil
}

// Apply does nothing for batch SQL captures.
func (BatchSQLDriver) Apply(ctx context.Context, req *pc.Request_Apply) (*pc.Response_Applied, error) {
	return &pc.Response_Applied{ActionDescription: ""}, nil
}

// Discover enumerates tables and views from `information_schema.tables` and generates
// placeholder capture queries for thos tables.
func (drv *BatchSQLDriver) Discover(ctx context.Context, req *pc.Request_Discover) (*pc.Response_Discovered, error) {
	var cfg Config
	if err := pf.UnmarshalStrict(req.ConfigJson, &cfg); err != nil {
		return nil, fmt.Errorf("parsing endpoint config: %w", err)
	}
	cfg.SetDefaults()

	var db, err = drv.Connect(ctx, &cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tables, err := discoverTables(ctx, db, cfg.Advanced.DiscoverSchemas)
	if err != nil {
		return nil, fmt.Errorf("error listing tables: %w", err)
	}
	keys, err := discoverPrimaryKeys(ctx, db, cfg.Advanced.DiscoverSchemas)
	if err != nil {
		return nil, fmt.Errorf("error listing primary keys: %w", err)
	}

	// We rebuild this map even though `discoverPrimaryKeys` already built and
	// discarded it, in order to keep the `discoverTables` / `discoverPrimaryKeys`
	// interface as stupidly simple as possible, which ought to make it just that
	// little bit easier to do this generically for multiple databases.
	var keysByTable = make(map[string]*discoveredPrimaryKey)
	for _, key := range keys {
		keysByTable[key.Schema+"."+key.Table] = key
	}

	var bindings []*pc.Response_Discovered_Binding
	for _, table := range tables {
		var tableID = table.Schema + "." + table.Name

		var recommendedName = recommendedCatalogName(table.Schema, table.Name)
		var res, err = drv.GenerateResource(recommendedName, table.Schema, table.Name, table.Type)
		if err != nil {
			log.WithFields(log.Fields{
				"reason": err,
				"table":  tableID,
				"type":   table.Type,
			}).Warn("unable to generate resource spec")
			continue
		}
		resourceConfigJSON, err := json.Marshal(res)
		if err != nil {
			return nil, fmt.Errorf("error serializing resource spec: %w", err)
		}

		// Try to generate a useful collection schema, but on error fall back to the
		// minimal schema with the default key [/_meta/polled, /_meta/index].
		var collectionSchema = minimalSchema
		var collectionKey = fallbackKey
		if tableKey, ok := keysByTable[tableID]; ok {
			if generatedSchema, err := generateCollectionSchema(tableKey.Columns, tableKey.ColumnTypes); err == nil {
				collectionSchema = generatedSchema
				collectionKey = nil
				for _, colName := range tableKey.Columns {
					collectionKey = append(collectionKey, primaryKeyToCollectionKey(colName))
				}
			} else {
				log.WithFields(log.Fields{"table": tableID, "err": err}).Warn("unable to generate collection schema")
			}
		}

		bindings = append(bindings, &pc.Response_Discovered_Binding{
			RecommendedName:    recommendedName,
			ResourceConfigJson: resourceConfigJSON,
			DocumentSchemaJson: collectionSchema,
			Key:                collectionKey,
			ResourcePath:       []string{res.Name},
		})
	}

	return &pc.Response_Discovered{Bindings: bindings}, nil
}

// primaryKeyToCollectionKey converts a database primary key column name into a Flow collection key
// JSON pointer with escaping for '~' and '/' applied per RFC6901.
func primaryKeyToCollectionKey(key string) string {
	// Any encoded '~' must be escaped first to prevent a second escape on escaped '/' values as
	// '~1'.
	key = strings.ReplaceAll(key, "~", "~0")
	key = strings.ReplaceAll(key, "/", "~1")
	return "/" + key
}

type discoveredTable struct {
	Schema string
	Name   string
	Type   string // Usually 'BASE TABLE' or 'VIEW'
}

func discoverTables(ctx context.Context, db *sql.DB, discoverSchemas []string) ([]*discoveredTable, error) {
	var query = new(strings.Builder)
	var args []any

	fmt.Fprintf(query, "SELECT n.nspname AS table_schema,")
	fmt.Fprintf(query, "       c.relname AS table_name,")
	fmt.Fprintf(query, "       CASE")
	fmt.Fprintf(query, "         WHEN n.oid = pg_my_temp_schema() THEN 'LOCAL TEMPORARY'::text")
	fmt.Fprintf(query, "         WHEN c.relkind = ANY (ARRAY['r'::\"char\", 'p'::\"char\"]) THEN 'BASE TABLE'::text")
	fmt.Fprintf(query, "         WHEN c.relkind = 'v'::\"char\" THEN 'VIEW'::text")
	fmt.Fprintf(query, "         WHEN c.relkind = 'f'::\"char\" THEN 'FOREIGN'::text")
	fmt.Fprintf(query, "         ELSE ''::text")
	fmt.Fprintf(query, "       END::information_schema.character_data AS table_type")
	fmt.Fprintf(query, " FROM pg_catalog.pg_class c")
	fmt.Fprintf(query, " JOIN pg_catalog.pg_namespace n ON (n.oid = c.relnamespace)")
	fmt.Fprintf(query, " WHERE n.nspname NOT IN ('pg_catalog', 'pg_internal', 'information_schema', 'catalog_history', 'cron')")
	fmt.Fprintf(query, "  AND c.relkind IN ('r', 'p', 'v', 'f')")
	if len(discoverSchemas) > 0 {
		fmt.Fprintf(query, "  AND n.nspname = ANY ($1)")
		args = append(args, discoverSchemas)
	}
	fmt.Fprintf(query, "  AND NOT c.relispartition;") // Exclude subpartitions of a partitioned table from discovery

	rows, err := db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("error executing discovery query %q: %w", query.String(), err)
	}
	defer rows.Close()

	var tables []*discoveredTable
	for rows.Next() {
		var tableSchema, tableName, tableType string
		if err := rows.Scan(&tableSchema, &tableName, &tableType); err != nil {
			return nil, fmt.Errorf("error scanning result row: %w", err)
		}
		tables = append(tables, &discoveredTable{
			Schema: tableSchema,
			Name:   tableName,
			Type:   tableType,
		})
	}
	return tables, nil
}

type discoveredPrimaryKey struct {
	Schema      string
	Table       string
	Columns     []string
	ColumnTypes map[string]*jsonschema.Schema
}

func discoverPrimaryKeys(ctx context.Context, db *sql.DB, discoverSchemas []string) ([]*discoveredPrimaryKey, error) {
	var query = new(strings.Builder)
	var args []any

	fmt.Fprintf(query, "SELECT result.TABLE_SCHEM, result.TABLE_NAME, result.COLUMN_NAME, result.KEY_SEQ, result.TYPE_NAME")
	fmt.Fprintf(query, " FROM (")
	fmt.Fprintf(query, "  SELECT n.nspname AS TABLE_SCHEM,")
	fmt.Fprintf(query, "   ct.relname AS TABLE_NAME, a.attname AS COLUMN_NAME,")
	fmt.Fprintf(query, "   (information_schema._pg_expandarray(i.indkey)).n AS KEY_SEQ, ci.relname AS PK_NAME,")
	fmt.Fprintf(query, "   information_schema._pg_expandarray(i.indkey) AS KEYS, a.attnum AS A_ATTNUM,")
	fmt.Fprintf(query, "   t.typname AS TYPE_NAME")
	fmt.Fprintf(query, "  FROM pg_catalog.pg_class ct")
	fmt.Fprintf(query, "   JOIN pg_catalog.pg_attribute a ON (ct.oid = a.attrelid)")
	fmt.Fprintf(query, "   JOIN pg_catalog.pg_namespace n ON (ct.relnamespace = n.oid)")
	fmt.Fprintf(query, "   JOIN pg_catalog.pg_index i ON (a.attrelid = i.indrelid)")
	fmt.Fprintf(query, "   JOIN pg_catalog.pg_class ci ON (ci.oid = i.indexrelid)")
	fmt.Fprintf(query, "   JOIN pg_catalog.pg_type t ON (a.atttypid = t.oid)")
	fmt.Fprintf(query, "  WHERE i.indisprimary")
	if len(discoverSchemas) > 0 {
		fmt.Fprintf(query, "   AND n.nspname = ANY ($1)")
		args = append(args, discoverSchemas)
	}
	fmt.Fprintf(query, " ) result")
	fmt.Fprintf(query, " WHERE result.A_ATTNUM = (result.KEYS).x")
	fmt.Fprintf(query, " ORDER BY result.table_name, result.pk_name, result.key_seq;")

	rows, err := db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("error executing discovery query %q: %w", query.String(), err)
	}
	defer rows.Close()

	var keysByTable = make(map[string]*discoveredPrimaryKey)
	for rows.Next() {
		var tableSchema, tableName, columnName, dataType string
		var ordinalPosition int
		if err := rows.Scan(&tableSchema, &tableName, &columnName, &ordinalPosition, &dataType); err != nil {
			return nil, fmt.Errorf("error scanning result row: %w", err)
		}

		var tableID = tableSchema + "." + tableName
		var keyInfo = keysByTable[tableID]
		if keyInfo == nil {
			keyInfo = &discoveredPrimaryKey{
				Schema:      tableSchema,
				Table:       tableName,
				ColumnTypes: make(map[string]*jsonschema.Schema),
			}
			keysByTable[tableID] = keyInfo
		}
		keyInfo.Columns = append(keyInfo.Columns, columnName)
		if jsonSchema, ok := databaseTypeToJSON[dataType]; ok {
			keyInfo.ColumnTypes[columnName] = jsonSchema
		} else {
			// Assume unknown types are strings, because it's almost always a string.
			// (Or it's an object, but those can't be Flow collection keys anyway)
			keyInfo.ColumnTypes[columnName] = &jsonschema.Schema{Type: "string"}
		}
		if ordinalPosition != len(keyInfo.Columns) {
			return nil, fmt.Errorf("primary key column %q (of table %q) appears out of order", columnName, tableID)
		}
	}

	var keys []*discoveredPrimaryKey
	for _, key := range keysByTable {
		keys = append(keys, key)
	}
	return keys, nil
}

var databaseTypeToJSON = map[string]*jsonschema.Schema{
	"int2":    {Type: "integer"},
	"int4":    {Type: "integer"},
	"int8":    {Type: "integer"},
	"bool":    {Type: "boolean"},
	"varchar": {Type: "string"},
}

var catalogNameSanitizerRe = regexp.MustCompile(`(?i)[^a-z0-9\-_.]`)

func recommendedCatalogName(schema, table string) string {
	var catalogName string
	// Omit 'default schema' names for Postgres and SQL Server. There is
	// no default schema for MySQL databases.
	if schema == "public" || schema == "dbo" {
		catalogName = table
	} else {
		catalogName = schema + "_" + table
	}
	return catalogNameSanitizerRe.ReplaceAllString(strings.ToLower(catalogName), "_")
}

// Validate checks that the configuration appears correct and that we can connect
// to the database and execute queries.
func (drv *BatchSQLDriver) Validate(ctx context.Context, req *pc.Request_Validate) (*pc.Response_Validated, error) {
	// Unmarshal the configuration and verify that we can connect to the database
	var cfg Config
	if err := pf.UnmarshalStrict(req.ConfigJson, &cfg); err != nil {
		return nil, fmt.Errorf("parsing endpoint config: %w", err)
	}
	cfg.SetDefaults()

	var db, err = drv.Connect(ctx, &cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Unmarshal and validate resource bindings to make sure they're well-formed too.
	var out []*pc.Response_Validated_Binding
	for _, binding := range req.Bindings {
		var res Resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return nil, fmt.Errorf("parsing resource config: %w", err)
		}

		out = append(out, &pc.Response_Validated_Binding{
			ResourcePath: []string{res.Name},
		})
	}
	return &pc.Response_Validated{Bindings: out}, nil
}

// Pull is the heart of a capture connector and outputs a neverending stream of documents.
func (drv *BatchSQLDriver) Pull(open *pc.Request_Open, stream *boilerplate.PullOutput) error {
	var cfg Config
	if err := pf.UnmarshalStrict(open.Capture.ConfigJson, &cfg); err != nil {
		return fmt.Errorf("parsing endpoint config: %w", err)
	}
	cfg.SetDefaults()

	var db, err = drv.Connect(stream.Context(), &cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	var bindings []bindingInfo
	for idx, binding := range open.Capture.Bindings {
		var res Resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return fmt.Errorf("parsing resource config: %w", err)
		}
		bindings = append(bindings, bindingInfo{
			resource: res,
			index:    idx,
			stateKey: boilerplate.StateKey(binding.StateKey),
		})
	}

	var state captureState
	if open.StateJson != nil {
		if err := pf.UnmarshalStrict(open.StateJson, &state); err != nil {
			return fmt.Errorf("parsing state checkpoint: %w", err)
		}
	}

	state, err = updateResourceStates(state, bindings)
	if err != nil {
		return fmt.Errorf("error initializing resource states: %w", err)
	}

	if err := stream.Ready(false); err != nil {
		return err
	}

	var capture = &capture{
		Config:               &cfg,
		State:                &state,
		DB:                   db,
		Bindings:             bindings,
		Output:               stream,
		TranslateValue:       drv.TranslateValue,
		DefaultQueryTemplate: drv.DefaultQueryTemplate,
	}
	return capture.Run(stream.Context())
}

func updateResourceStates(prevState captureState, bindings []bindingInfo) (captureState, error) {
	var newState = captureState{
		Streams: make(map[boilerplate.StateKey]*streamState),
	}
	for _, binding := range bindings {
		var sk = binding.stateKey
		var res = binding.resource
		var stream = prevState.Streams[sk]
		if stream != nil && !slices.Equal(stream.CursorNames, res.Cursor) {
			log.WithFields(log.Fields{
				"name": res.Name,
				"prev": stream.CursorNames,
				"next": res.Cursor,
			}).Warn("cursor columns changed, resetting stream state")
			stream = nil
		}
		if stream == nil {
			stream = &streamState{CursorNames: res.Cursor}
		}
		newState.Streams[sk] = stream
	}
	return newState, nil
}

type capture struct {
	Config               *Config
	State                *captureState
	DB                   *sql.DB
	Bindings             []bindingInfo
	Output               *boilerplate.PullOutput
	TranslateValue       func(val any, databaseTypeName string) (any, error)
	DefaultQueryTemplate string
}

type bindingInfo struct {
	resource Resource
	index    int
	stateKey boilerplate.StateKey
}

type captureState struct {
	Streams map[boilerplate.StateKey]*streamState `json:"bindingStateV1,omitempty"`
}

type streamState struct {
	CursorNames  []string
	CursorValues []any
	LastPolled   time.Time
}

func (s *captureState) Validate() error {
	return nil
}

func (c *capture) Run(ctx context.Context) error {
	var eg, workerCtx = errgroup.WithContext(ctx)
	for idx, binding := range c.Bindings {
		if idx > 0 {
			// Slightly stagger worker thread startup. Five seconds should be long
			// enough for most fast queries to complete their first execution, and
			// the hope is this reduces peak load on both the database and us.
			time.Sleep(5 * time.Second)
		}
		var binding = binding // Copy for goroutine closure
		eg.Go(func() error { return c.worker(workerCtx, &binding) })
	}
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("capture terminated with error: %w", err)
	}
	return nil
}

func (c *capture) worker(ctx context.Context, binding *bindingInfo) error {
	var res = binding.resource
	log.WithFields(log.Fields{
		"name":   res.Name,
		"tmpl":   res.Template,
		"cursor": res.Cursor,
		"poll":   res.PollSchedule,
	}).Info("starting worker")

	var templateString = res.Template
	if templateString == "" {
		templateString = c.DefaultQueryTemplate
	}
	var queryTemplate, err = template.New("query").Funcs(templateFuncs).Parse(templateString)
	if err != nil {
		return fmt.Errorf("error parsing template: %w", err)
	}

	for ctx.Err() == nil {
		if err := c.poll(ctx, binding, queryTemplate); err != nil {
			return fmt.Errorf("error polling binding %q: %w", res.Name, err)
		}
	}
	return ctx.Err()
}

func (c *capture) poll(ctx context.Context, binding *bindingInfo, tmpl *template.Template) error {
	var res = binding.resource
	var stateKey = binding.stateKey
	var state, ok = c.State.Streams[stateKey]
	if !ok {
		return fmt.Errorf("internal error: no state for stream %q", res.Name)
	}
	var cursorNames = state.CursorNames
	var cursorValues = state.CursorValues

	var quotedCursorNames []string
	for _, cursorName := range cursorNames {
		quotedCursorNames = append(quotedCursorNames, quoteIdentifier(cursorName))
	}

	var templateArg = map[string]any{
		"IsFirstQuery": len(cursorValues) == 0,
		"CursorFields": quotedCursorNames,
		"SchemaName":   res.SchemaName,
		"TableName":    res.TableName,
	}

	// Polling schedule can be configured per binding. If unset, falls back to the
	// connector global polling schedule.
	var pollScheduleStr = c.Config.Advanced.PollSchedule
	if res.PollSchedule != "" {
		pollScheduleStr = res.PollSchedule
	}
	var pollSchedule, err = schedule.Parse(pollScheduleStr)
	if err != nil {
		return fmt.Errorf("failed to parse polling schedule %q: %w", pollScheduleStr, err)
	}
	log.WithFields(log.Fields{
		"name": res.Name,
		"poll": pollScheduleStr,
	}).Info("waiting for next scheduled poll")
	if err := schedule.WaitForNext(ctx, pollSchedule, state.LastPolled); err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"name": res.Name,
		"poll": pollScheduleStr,
		"prev": state.LastPolled.Format(time.RFC3339Nano),
	}).Info("ready to poll")

	var queryBuf = new(strings.Builder)
	if err := tmpl.Execute(queryBuf, templateArg); err != nil {
		return fmt.Errorf("error generating query: %w", err)
	}
	var query = queryBuf.String()

	log.WithFields(log.Fields{
		"query": query,
		"args":  cursorValues,
	}).Info("executing query")
	var pollTime = time.Now().UTC()
	state.LastPolled = pollTime

	rows, err := c.DB.QueryContext(ctx, query, cursorValues...)
	if err != nil {
		return fmt.Errorf("error executing query: %w", err)
	}
	defer rows.Close()

	columnNames, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("error processing query result: %w", err)
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return fmt.Errorf("error processing query result: %w", err)
	}
	for i, columnType := range columnTypes {
		log.WithFields(log.Fields{
			"idx":          i,
			"name":         columnType.DatabaseTypeName(),
			"scanTypeName": columnType.ScanType().Name(),
		}).Debug("column type")
	}

	var columnIndices = make(map[string]int)
	for idx, name := range columnNames {
		columnIndices[name] = idx
	}
	var cursorIndices []int
	for _, cursorName := range cursorNames {
		cursorIndices = append(cursorIndices, columnIndices[cursorName])
	}

	var columnValues = make([]any, len(columnNames))
	var columnPointers = make([]any, len(columnValues))
	for i := range columnPointers {
		columnPointers[i] = &columnValues[i]
	}

	var shape = encrow.NewShape(append(columnNames, "_meta"))
	var rowValues = make([]any, len(columnNames)+1)
	var serializedDocument []byte

	var count int
	for rows.Next() {
		if err := rows.Scan(columnPointers...); err != nil {
			return fmt.Errorf("error scanning result row: %w", err)
		}

		for idx, val := range columnValues {
			var translatedVal, err = c.TranslateValue(val, columnTypes[idx].DatabaseTypeName())
			if err != nil {
				return fmt.Errorf("error translating column %q value: %w", columnNames[idx], err)
			}
			rowValues[idx] = translatedVal
		}
		rowValues[len(rowValues)-1] = &documentMetadata{
			Polled: pollTime,
			Index:  count,
		}

		serializedDocument, err = shape.Encode(serializedDocument, rowValues)
		if err != nil {
			return fmt.Errorf("error serializing document: %w", err)
		} else if err := c.Output.Documents(binding.index, serializedDocument); err != nil {
			return fmt.Errorf("error emitting document: %w", err)
		}

		if len(cursorValues) != len(cursorNames) {
			// Allocate a new values list if needed. This is done inside of the loop, so
			// an empty result set will be a no-op even when the previous state is nil.
			cursorValues = make([]any, len(cursorNames))
		}
		for i, j := range cursorIndices {
			cursorValues[i] = rowValues[j]
		}
		state.CursorValues = cursorValues

		count++
	}

	if err := c.streamStateCheckpoint(stateKey, state); err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"query": query,
		"count": count,
	}).Info("query complete")
	return nil
}

func (c *capture) streamStateCheckpoint(sk boilerplate.StateKey, state *streamState) error {
	var checkpointPatch = captureState{Streams: make(map[boilerplate.StateKey]*streamState)}
	checkpointPatch.Streams[sk] = state

	if checkpointJSON, err := json.Marshal(checkpointPatch); err != nil {
		return fmt.Errorf("error serializing state checkpoint: %w", err)
	} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
		return fmt.Errorf("error emitting checkpoint: %w", err)
	}
	return nil
}

func quoteIdentifier(name string) string {
	// From https://www.postgresql.org/docs/14/sql-syntax-lexical.html#SQL-SYNTAX-IDENTIFIERS:
	//
	//     Quoted identifiers can contain any character, except the character with code zero.
	//     (To include a double quote, write two double quotes.)
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
