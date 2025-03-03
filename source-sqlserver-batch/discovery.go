package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	pc "github.com/estuary/flow/go/protocols/capture"
	pf "github.com/estuary/flow/go/protocols/flow"
	"github.com/invopop/jsonschema"
	log "github.com/sirupsen/logrus"
)

// The fallback key of discovered collections when the source table has no primary key.
var fallbackKey = []string{"/_meta/row_id"}

func generateCollectionSchema(cfg *Config, keyColumns []string, columnTypes map[string]*jsonschema.Schema) (json.RawMessage, error) {
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
	for colName, colType := range columnTypes {
		properties[colName] = colType
	}
	required = append(required, keyColumns...)

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
	columns, err := discoverColumns(ctx, db, cfg.Advanced.DiscoverSchemas)
	if err != nil {
		return nil, fmt.Errorf("error listing columns: %w", err)
	}
	keys, err := discoverPrimaryKeys(ctx, db, cfg.Advanced.DiscoverSchemas)
	if err != nil {
		return nil, fmt.Errorf("error listing primary keys: %w", err)
	}

	var bindings []*pc.Response_Discovered_Binding
	for _, table := range tables {
		var tableID = table.Schema + "." + table.Name

		var recommendedName = recommendedCatalogName(table.Schema, table.Name)
		var res, err = drv.GenerateResource(&cfg, recommendedName, table.Schema, table.Name, table.Type)
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

		// Figure out column types and collection key, if any
		var columnTypes = make(map[string]*jsonschema.Schema)
		for _, col := range columns[tableID] {
			columnTypes[col.Name] = col.DataType.JSONSchema()
		}
		var keyColumns []string    // Names of primary-key columns in the source table. Empty if no primary key.
		var collectionKey []string // JSON pointers of the collection key
		if tableKey, ok := keys[tableID]; ok {
			keyColumns = tableKey.Columns
			for _, colName := range tableKey.Columns {
				collectionKey = append(collectionKey, primaryKeyToCollectionKey(colName))
			}
		} else {
			collectionKey = fallbackKey
		}

		// Generate schema for the collection.
		collectionSchema, err := generateCollectionSchema(&cfg, keyColumns, columnTypes)
		if err != nil {
			return nil, fmt.Errorf("error generating minimal collection schema: %w", err)
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

	fmt.Fprintf(query, "SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE")
	fmt.Fprintf(query, " FROM INFORMATION_SCHEMA.TABLES")
	fmt.Fprintf(query, " WHERE TABLE_NAME != 'SYSTRANSCHEMAS'")
	if len(discoverSchemas) > 0 {
		fmt.Fprintf(query, " AND TABLE_SCHEMA IN (")
		for i, schema := range discoverSchemas {
			if i > 0 {
				fmt.Fprintf(query, ", ")
			}
			fmt.Fprintf(query, "@p%d", i+1)
			args = append(args, schema)
		}
		fmt.Fprintf(query, ")")
	} else {
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'INFORMATION_SCHEMA'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'PERFORMANCE_SCHEMA'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'SYS'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'CDC'")
	}
	fmt.Fprintf(query, ";")

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

type discoveredColumn struct {
	Schema      string     // The schema in which the table resides
	Table       string     // The name of the table with this column
	Name        string     // The name of the column
	Index       int        // The ordinal position of the column within a row
	IsNullable  bool       // Whether the column can be null
	DataType    columnType // The datatype of the column
	Description *string    // The description of the column, if present and known
}

type columnType interface {
	JSONSchema() *jsonschema.Schema
}

type basicColumnType struct {
	jsonTypes       []string
	contentEncoding string
	format          string
	nullable        bool
	description     string
}

func (ct *basicColumnType) JSONSchema() *jsonschema.Schema {
	var sch = &jsonschema.Schema{
		Format: ct.format,
		Extras: make(map[string]interface{}),
	}

	if ct.contentEncoding != "" {
		sch.Extras["contentEncoding"] = ct.contentEncoding // New in 2019-09.
	}

	if ct.jsonTypes != nil {
		var types = append([]string(nil), ct.jsonTypes...)
		if ct.nullable {
			types = append(types, "null")
		}
		if len(types) == 1 {
			sch.Type = types[0]
		} else {
			sch.Extras["type"] = types
		}
	}
	return sch
}

func discoverColumns(ctx context.Context, db *sql.DB, discoverSchemas []string) (map[string][]*discoveredColumn, error) {
	var query = new(strings.Builder)
	var args []any

	fmt.Fprintf(query, "SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION,")
	fmt.Fprintf(query, " CASE WHEN IS_NULLABLE = 'YES' THEN 1 ELSE 0 END,")
	fmt.Fprintf(query, " DATA_TYPE")
	fmt.Fprintf(query, " FROM INFORMATION_SCHEMA.COLUMNS")
	fmt.Fprintf(query, " WHERE TABLE_NAME != 'SYSTRANSCHEMAS'")
	if len(discoverSchemas) > 0 {
		fmt.Fprintf(query, " AND TABLE_SCHEMA IN (")
		for i, schema := range discoverSchemas {
			if i > 0 {
				fmt.Fprintf(query, ", ")
			}
			fmt.Fprintf(query, "@p%d", i+1)
			args = append(args, schema)
		}
		fmt.Fprintf(query, ")")
	} else {
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'INFORMATION_SCHEMA'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'PERFORMANCE_SCHEMA'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'SYS'")
		fmt.Fprintf(query, " AND TABLE_SCHEMA != 'CDC'")
	}
	fmt.Fprintf(query, " ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION;")

	rows, err := db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("error executing discovery query %q: %w", query.String(), err)
	}
	defer rows.Close()

	var columnsByTable = make(map[string][]*discoveredColumn)
	for rows.Next() {
		var tableSchema, tableName, columnName string
		var columnIndex int
		var isNullable bool
		var typeName string
		if err := rows.Scan(&tableSchema, &tableName, &columnName, &columnIndex, &isNullable, &typeName); err != nil {
			return nil, fmt.Errorf("error scanning result row: %w", err)
		}

		var dataType, ok = databaseTypeToJSON[typeName]
		if !ok {
			dataType = basicColumnType{description: fmt.Sprintf("using catch-all schema for unknown type %q", typeName)}
		}
		dataType.nullable = isNullable

		var tableID = tableSchema + "." + tableName
		var column = &discoveredColumn{
			Schema:     tableSchema,
			Table:      tableName,
			Name:       columnName,
			Index:      columnIndex,
			IsNullable: isNullable,
			DataType:   &dataType,
		}
		columnsByTable[tableID] = append(columnsByTable[tableID], column)
	}
	return columnsByTable, nil
}

type discoveredPrimaryKey struct {
	Schema  string
	Table   string
	Columns []string
}

func discoverPrimaryKeys(ctx context.Context, db *sql.DB, discoverSchemas []string) (map[string]*discoveredPrimaryKey, error) {
	var query = new(strings.Builder)
	var args []any

	fmt.Fprintf(query, "SELECT KCU.TABLE_SCHEMA, KCU.TABLE_NAME, KCU.COLUMN_NAME, KCU.ORDINAL_POSITION")
	fmt.Fprintf(query, " FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE KCU")
	fmt.Fprintf(query, " JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS TCS")
	fmt.Fprintf(query, " ON TCS.CONSTRAINT_CATALOG = KCU.CONSTRAINT_CATALOG")
	fmt.Fprintf(query, " AND TCS.CONSTRAINT_SCHEMA = KCU.CONSTRAINT_SCHEMA")
	fmt.Fprintf(query, " AND TCS.CONSTRAINT_NAME = KCU.CONSTRAINT_NAME")
	fmt.Fprintf(query, " AND TCS.TABLE_CATALOG = KCU.TABLE_CATALOG")
	fmt.Fprintf(query, " AND TCS.TABLE_SCHEMA = KCU.TABLE_SCHEMA")
	fmt.Fprintf(query, " AND TCS.TABLE_NAME = KCU.TABLE_NAME")
	fmt.Fprintf(query, " WHERE TCS.CONSTRAINT_TYPE = 'PRIMARY KEY'")
	fmt.Fprintf(query, " AND KCU.TABLE_NAME != 'SYSTRANSCHEMAS'")
	if len(discoverSchemas) > 0 {
		fmt.Fprintf(query, " AND KCU.TABLE_SCHEMA IN (")
		for i, schema := range discoverSchemas {
			if i > 0 {
				fmt.Fprintf(query, ", ")
			}
			fmt.Fprintf(query, "@p%d", i+1)
			args = append(args, schema)
		}
		fmt.Fprintf(query, ")")
	} else {
		fmt.Fprintf(query, " AND KCU.TABLE_SCHEMA != 'INFORMATION_SCHEMA'")
		fmt.Fprintf(query, " AND KCU.TABLE_SCHEMA != 'PERFORMANCE_SCHEMA'")
		fmt.Fprintf(query, " AND KCU.TABLE_SCHEMA != 'SYS'")
		fmt.Fprintf(query, " AND KCU.TABLE_SCHEMA != 'CDC'")
	}
	fmt.Fprintf(query, " ORDER BY KCU.TABLE_SCHEMA, KCU.TABLE_NAME, KCU.ORDINAL_POSITION;")

	rows, err := db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("error executing discovery query %q: %w", query.String(), err)
	}
	defer rows.Close()

	var keysByTable = make(map[string]*discoveredPrimaryKey)
	for rows.Next() {
		var tableSchema, tableName, columnName string
		var ordinalPosition int
		if err := rows.Scan(&tableSchema, &tableName, &columnName, &ordinalPosition); err != nil {
			return nil, fmt.Errorf("error scanning result row: %w", err)
		}

		var tableID = tableSchema + "." + tableName
		var keyInfo = keysByTable[tableID]
		if keyInfo == nil {
			keyInfo = &discoveredPrimaryKey{Schema: tableSchema, Table: tableName}
			keysByTable[tableID] = keyInfo
		}
		keyInfo.Columns = append(keyInfo.Columns, columnName)
		if ordinalPosition != len(keyInfo.Columns) {
			return nil, fmt.Errorf("primary key column %q (of table %q) appears out of order", columnName, tableID)
		}
	}
	return keysByTable, nil
}

var databaseTypeToJSON = map[string]basicColumnType{
	// Numeric types
	"bit":        {jsonTypes: []string{"boolean"}},
	"tinyint":    {jsonTypes: []string{"integer"}},
	"smallint":   {jsonTypes: []string{"integer"}},
	"int":        {jsonTypes: []string{"integer"}},
	"bigint":     {jsonTypes: []string{"integer"}},
	"float":      {jsonTypes: []string{"number"}},
	"real":       {jsonTypes: []string{"number"}},
	"numeric":    {jsonTypes: []string{"string"}, format: "number"},
	"decimal":    {jsonTypes: []string{"string"}, format: "number"},
	"money":      {jsonTypes: []string{"string"}, format: "number"},
	"smallmoney": {jsonTypes: []string{"string"}, format: "number"},

	// String types
	"char":     {jsonTypes: []string{"string"}},
	"varchar":  {jsonTypes: []string{"string"}},
	"text":     {jsonTypes: []string{"string"}},
	"nchar":    {jsonTypes: []string{"string"}},
	"nvarchar": {jsonTypes: []string{"string"}},
	"ntext":    {jsonTypes: []string{"string"}},
	"xml":      {jsonTypes: []string{"string"}},

	// Binary types
	"binary":    {jsonTypes: []string{"string"}, contentEncoding: "base64"},
	"varbinary": {jsonTypes: []string{"string"}, contentEncoding: "base64"},
	"image":     {jsonTypes: []string{"string"}, contentEncoding: "base64"},

	// Date/Time types
	"date":           {jsonTypes: []string{"string"}, format: "date"},
	"time":           {jsonTypes: []string{"string"}, format: "time"},
	"datetime":       {jsonTypes: []string{"string"}, format: "date-time"},
	"datetime2":      {jsonTypes: []string{"string"}, format: "date-time"},
	"smalldatetime":  {jsonTypes: []string{"string"}, format: "date-time"},
	"datetimeoffset": {jsonTypes: []string{"string"}, format: "date-time"},

	// Other types
	"uniqueidentifier": {jsonTypes: []string{"string"}, format: "uuid"},
}

var catalogNameSanitizerRe = regexp.MustCompile(`(?i)[^a-z0-9\-_.]`)

func recommendedCatalogName(schema, table string) string {
	var catalogName string
	if schema == "dbo" {
		catalogName = table
	} else {
		catalogName = schema + "_" + table
	}
	return catalogNameSanitizerRe.ReplaceAllString(strings.ToLower(catalogName), "_")
}
