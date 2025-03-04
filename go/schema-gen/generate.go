package schemagen

import (
	"reflect"
	"strconv"

	"github.com/invopop/jsonschema"
)

func GenerateSchema(title string, configObject interface{}) *jsonschema.Schema {
	// By default, the library generates schemas with a top-level $ref that references a definition.
	// That breaks UI code that tries to generate forms from the schemas, and is just weird and
	// silly anyway. While we're at it, we just disable references altogether, since they tend to
	// hurt readability more than they help for these schemas.
	var reflector = jsonschema.Reflector{
		ExpandedStruct: true,
		DoNotReference: true,
	}
	var schema = reflector.ReflectFromType(reflect.TypeOf(configObject))
	schema.AdditionalProperties = nil // Unset means additional properties are permitted on the root object, as they should be
	schema.Definitions = nil          // Since no references are used, these definitions are just noise
	schema.Title = title
	walkSchema(
		schema,
		fixSchemaFlagBools("secret", "advanced", "multiline", "x-collection-name", "x-schema-name", "x-delta-updates"),
		fixSchemaOrderingStrings,
	)

	return schema
}

// walkSchema invokes visit on every property of the root schema, and then traverses each of these
// sub-schemas recursively. The visit function should modify the provided schema in-place to
// accomplish the desired transformation.
func walkSchema(root *jsonschema.Schema, visits ...func(t *jsonschema.Schema)) {
	if root.Properties != nil {
		for pair := root.Properties.Oldest(); pair != nil; pair = pair.Next() {
			for _, visit := range visits {
				visit(pair.Value)
			}

			walkSchema(pair.Value, visits...)
		}
	}
}

func fixSchemaFlagBools(flagKeys ...string) func(t *jsonschema.Schema) {
	return func(t *jsonschema.Schema) {
		for key, val := range t.Extras {
			for _, flag := range flagKeys {
				if key != flag {
					continue
				} else if val == "true" {
					t.Extras[key] = true
				} else if val == "false" {
					t.Extras[key] = false
				}
			}
		}
	}
}

func fixSchemaOrderingStrings(t *jsonschema.Schema) {
	for key, val := range t.Extras {
		if key == "order" {
			if str, ok := val.(string); ok {
				converted, err := strconv.Atoi(str)
				if err != nil {
					// Don't try to convert strings that don't look like integers.
					continue
				}
				t.Extras[key] = converted
			}
		}
	}
}
