#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

config_json_template='{
    "token": "${MOTHERDUCK_TOKEN}",
    "database": "${MOTHERDUCK_DATABASE}",
    "schema": "${MOTHERDUCK_SCHEMA}",
    "bucket": "${MOTHERDUCK_BUCKET}",
    "awsAccessKeyId": "${AWS_ACCESS_KEY_ID}",
    "awsSecretAccessKey": "${AWS_SECRET_ACCESS_KEY}",
    "region": "${AWS_REGION}"
}'

resources_json_template='[
  {
    "resource": {
      "table": "simple"
    },
    "source": "${TEST_COLLECTION_SIMPLE}"
  },
  {
    "resource": {
      "table": "duplicate_keys_standard"
    },
    "source": "${TEST_COLLECTION_DUPLICATED_KEYS}"
  },
  {
    "resource": {
      "table": "duplicate_keys_delta",
      "delta_updates": true
    },
    "source": "${TEST_COLLECTION_DUPLICATED_KEYS}"
  },
  {
    "resource": {
      "table": "duplicate_keys_delta_exclude_flow_doc",
      "delta_updates": true
    },
    "source": "${TEST_COLLECTION_DUPLICATED_KEYS}",
    "fields": {
      "recommended": true,
      "exclude": [
        "flow_document"
      ]
    }
  },
  {
    "resource": {
      "table": "multiple_types"
    },
    "source": "${TEST_COLLECTION_MULTIPLE_DATATYPES}",
    "fields": {
      "recommended": true,
      "exclude": ["nested/id"],
      "include": {
        "nested": {},
        "array_int": {},
        "multiple": {}
      }
    }
  },
  {
    "resource": {
      "table": "formatted_strings"
    },
    "source": "${TEST_COLLECTION_FORMATTED_STRINGS}",
    "fields": {
      "recommended": true
    }
  },
  {
    "resource": {
      "table": "unsigned_bigint"
    },
    "source": "${TEST_COLLECTION_UNSIGNED_BIGINT}"
  },
  {
    "resource": {
      "table": "deletions"
    },
    "source": "${TEST_COLLECTION_DELETIONS}"
  }
]'

STORAGE_TYPE="${STORAGE_TYPE:-s3}"
export CONNECTOR_CONFIG="$(decrypt_config ${TEST_DIR}/${CONNECTOR}/config.${STORAGE_TYPE}.yaml)"
export MOTHERDUCK_DATABASE="$(echo $CONNECTOR_CONFIG | jq -r .database)"
export MOTHERDUCK_SCHEMA="$(echo $CONNECTOR_CONFIG | jq -r .schema)"
export motherduck_token="$(echo $CONNECTOR_CONFIG | jq -r .token)"

export RESOURCES_CONFIG="$(echo "$resources_json_template" | envsubst | jq -c)"
