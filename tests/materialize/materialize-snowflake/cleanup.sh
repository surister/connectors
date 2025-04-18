#!/bin/bash

set -e

function dropTable() {
    go run ${TEST_DIR}/materialize-snowflake/fetch-data.go --delete "$1"
}

# Remove materialized tables.
dropTable "simple"
dropTable "duplicate_keys_standard"
dropTable "duplicate_keys_delta"
dropTable "duplicate_keys_delta_exclude_flow_doc"
dropTable "multiple_types"
dropTable "formatted_strings"
dropTable "symbols"
dropTable "unsigned_bigint"
dropTable "deletions"
dropTable "string_escaped_key"
