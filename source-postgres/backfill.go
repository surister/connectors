package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/estuary/connectors/sqlcapture"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/sirupsen/logrus"
)

var statementTimeoutRegexp = regexp.MustCompile(`canceling statement due to statement timeout`)

// ScanTableChunk fetches a chunk of rows from the specified table, resuming from `resumeKey` if non-nil.
func (db *postgresDatabase) ScanTableChunk(ctx context.Context, info *sqlcapture.DiscoveryInfo, state *sqlcapture.TableState, callback func(event *sqlcapture.ChangeEvent) error) (bool, []byte, error) {
	var keyColumns = state.KeyColumns
	var resumeAfter = state.Scanned
	var schema, table = info.Schema, info.Name
	var streamID = sqlcapture.JoinStreamID(schema, table)
	var logEntry = logrus.WithField("stream", streamID)

	var columnTypes = make(map[string]interface{})
	for name, column := range info.Columns {
		columnTypes[name] = column.DataType
	}

	var generatedColumns []string
	if details, ok := info.ExtraDetails.(*postgresTableDiscoveryDetails); ok {
		generatedColumns = details.GeneratedColumns
	}

	// Compute backfill query and arguments list
	var disableParallelWorkers bool
	var query string
	var args []any
	switch state.Mode {
	case sqlcapture.TableStateKeylessBackfill:
		var afterCTID = "(0,0)"
		if resumeAfter != nil {
			afterCTID = string(resumeAfter)
		}
		logEntry.WithField("ctid", afterCTID).Debug("scanning keyless table chunk")
		query = db.keylessScanQuery(info, schema, table)
		args = []any{afterCTID}
		disableParallelWorkers = true
	case sqlcapture.TableStatePreciseBackfill, sqlcapture.TableStateUnfilteredBackfill:
		var isPrecise = (state.Mode == sqlcapture.TableStatePreciseBackfill)
		if resumeAfter != nil {
			var resumeKey, err = sqlcapture.UnpackTuple(resumeAfter, decodeKeyFDB)
			if err != nil {
				return false, nil, fmt.Errorf("error unpacking resume key for %q: %w", streamID, err)
			}
			if len(resumeKey) != len(keyColumns) {
				return false, nil, fmt.Errorf("expected %d resume-key values but got %d", len(keyColumns), len(resumeKey))
			}
			logEntry.WithFields(logrus.Fields{
				"keyColumns": keyColumns,
				"resumeKey":  resumeKey,
			}).Debug("scanning subsequent table chunk")
			query = db.buildScanQuery(false, isPrecise, keyColumns, columnTypes, schema, table)
			args = resumeKey
		} else {
			logEntry.WithField("keyColumns", keyColumns).Debug("scanning initial table chunk")
			query = db.buildScanQuery(true, isPrecise, keyColumns, columnTypes, schema, table)
		}
	default:
		return false, nil, fmt.Errorf("invalid backfill mode %q", state.Mode)
	}

	// Keyless backfill queries need to return results in CTID order, but we can't ask
	// for that because `ORDER BY ctid` forces a sort, so we rely on it being true as an
	// implementation detail of how `WHERE ctid > $1` queries execute in practice. But
	// parallel query execution breaks that assumption, so we need to force the query
	// planner to not use parallel workers in such cases.
	if disableParallelWorkers {
		if _, err := db.conn.Exec(ctx, "SET max_parallel_workers_per_gather TO 0"); err != nil {
			logrus.WithField("err", err).Warn("error attempting to disable parallel workers")
		} else {
			defer func() {
				if _, err := db.conn.Exec(ctx, "SET max_parallel_workers_per_gather TO DEFAULT"); err != nil {
					logrus.WithField("err", err).Warn("error resetting max_parallel_workers to default")
				}
			}()
		}
	}

	// If this is the first chunk being backfilled, run an `EXPLAIN` on it and log the results
	db.explainQuery(ctx, streamID, query, args)

	// Execute the backfill query to fetch rows from the database
	logEntry.WithFields(logrus.Fields{"query": query, "args": args}).Debug("executing query")
	rows, err := db.conn.Query(ctx, query, args...)
	if err != nil {
		return false, nil, fmt.Errorf("unable to execute query %q: %w", query, err)
	}
	defer rows.Close()

	// Process the results into `changeEvent` structs and return them
	var cols = rows.FieldDescriptions()
	var totalRows, resultRows int // totalRows counts DB returned rows, resultRows counts rows after XID filtering
	var nextRowKey []byte         // The row key from which a subsequent backfill chunk should resume
	var rowOffset = state.BackfilledCount
	var prevTID pgtype.TID // Used when processing keyless backfill results to sanity-check ordering
	logEntry.Debug("translating query rows to change events")
	for rows.Next() {
		totalRows++

		// Scan the row values and copy into the equivalent map
		var vals, err = decodeRowValues(rows)
		if err != nil {
			return false, nil, fmt.Errorf("unable to get row values: %w", err)
		}
		var fields = make(map[string]interface{})
		for idx := range cols {
			fields[string(cols[idx].Name)] = vals[idx]
		}

		// Compute row key and update 'nextRowKey' before XID filtering, so that we can resume
		// correctly when doing an XMIN-filtered backfill even if no result rows satisfy the filter.
		var rowKey []byte
		if state.Mode == sqlcapture.TableStateKeylessBackfill {
			var ctid = fields["ctid"].(pgtype.TID)
			delete(fields, "ctid")

			if !ctid.Valid {
				return false, nil, fmt.Errorf("internal error: invalid ctid value %#v", ctid)
			}
			rowKey = []byte(fmt.Sprintf("(%d,%d)", ctid.BlockNumber, ctid.OffsetNumber))

			// Sanity check that rows are returned in ascending CTID order within a given backfill chunk
			if (ctid.BlockNumber < prevTID.BlockNumber) || ((ctid.BlockNumber == prevTID.BlockNumber) && (ctid.OffsetNumber <= prevTID.OffsetNumber)) {
				return false, nil, fmt.Errorf("internal error: ctid ordering sanity check failed: %v <= %v", ctid, prevTID)
			}
			prevTID = ctid
		} else {
			rowKey, err = sqlcapture.EncodeRowKey(keyColumns, fields, columnTypes, encodeKeyFDB)
			if err != nil {
				return false, nil, fmt.Errorf("error encoding row key for %q: %w", streamID, err)
			}
		}
		nextRowKey = rowKey

		// Filter result rows based on XMIN values
		if db.config.Advanced.MinimumBackfillXID != "" {
			xminStr, ok := fields["xmin"].(string)
			if !ok {
				return false, nil, fmt.Errorf("internal error: xmin column missing or not a string despite filtering being enabled")
			}
			xmin, err := strconv.ParseUint(xminStr, 10, 64)
			if err != nil {
				return false, nil, fmt.Errorf("invalid xmin value %q: %w", xminStr, err)
			}
			minXID, _ := strconv.ParseUint(db.config.Advanced.MinimumBackfillXID, 10, 64) // Error ignored as it's validated at config load

			if compareXID32(uint32(xmin), uint32(minXID)) < 0 {
				continue // Skip this row
			}
			delete(fields, "xmin") // Remove xmin from captured fields now
		}

		for _, name := range generatedColumns {
			// Generated column values cannot be captured via CDC, so we have to exclude them from
			// backfills as well or we could end up with incorrect/stale values. Better not to have
			// a property at all in those circumstances.
			delete(fields, name)
		}

		if err := db.translateRecordFields(info, fields); err != nil {
			return false, nil, fmt.Errorf("error backfilling table %q: %w", table, err)
		}

		var event = &sqlcapture.ChangeEvent{
			Operation: sqlcapture.InsertOp,
			RowKey:    rowKey,
			Source: &postgresSource{
				SourceCommon: sqlcapture.SourceCommon{
					Millis:   0, // Not known.
					Schema:   schema,
					Snapshot: true,
					Table:    table,
				},
				Location: [3]int{-1, rowOffset, 0},
			},
			Before: nil,
			After:  fields,
		}
		if err := callback(event); err != nil {
			return false, nil, fmt.Errorf("error processing change event: %w", err)
		}
		resultRows++ // Only increment for rows that passed the filter and were sent
		rowOffset++
	}

	if err := rows.Err(); err != nil {
		// As a special case, consider statement timeouts to be not an error so long as we got
		// at least one row back (totalRows > 0). This allows us to make partial progress on slow databases with
		// statement timeouts set, without having to fine-tune the backfill chunk size.
		if pgErr, ok := err.(*pgconn.PgError); ok && statementTimeoutRegexp.MatchString(pgErr.Message) && totalRows > 0 {
			logrus.WithFields(logrus.Fields{
				"stream":     streamID,
				"totalRows":  totalRows,
				"resultRows": resultRows,
			}).Warn("backfill query interrupted by statement timeout; partial progress saved")

			// Even if resultRows is 0, if totalRows > 0, we made progress scanning, so we don't return an error.
			// The next chunk will resume correctly based on the last row's key/ctid regardless of XID filtering.
			return false, nextRowKey, nil
		}
		return false, nil, err
	}

	// Backfill is complete if the *total* number of rows returned by the DB was less than the requested chunk size.
	// This correctly handles cases where the last chunk might be filtered down to zero rows by XMIN.
	var backfillComplete = totalRows < db.config.Advanced.BackfillChunkSize
	logEntry.WithFields(logrus.Fields{
		"totalRows":  totalRows,
		"resultRows": resultRows,
		"chunkSize":  db.config.Advanced.BackfillChunkSize,
		"complete":   backfillComplete,
	}).Debug("finished processing table chunk")
	return backfillComplete, nextRowKey, nil
}

// WriteWatermark writes the provided string into the 'watermarks' table.
func (db *postgresDatabase) WriteWatermark(ctx context.Context, watermark string) error {
	logrus.WithField("watermark", watermark).Debug("writing watermark")
	var query = fmt.Sprintf(`INSERT INTO %s (slot, watermark) VALUES ($1,$2) ON CONFLICT (slot) DO UPDATE SET watermark = $2;`, db.config.Advanced.WatermarksTable)
	var _, err = db.conn.Exec(ctx, query, db.config.Advanced.SlotName, watermark)
	if err != nil {
		return fmt.Errorf("error upserting new watermark for slot %q: %w", db.config.Advanced.SlotName, err)
	}
	return nil
}

// WatermarksTable returns the name of the table to which WriteWatermarks writes UUIDs.
func (db *postgresDatabase) WatermarksTable() string {
	return db.config.Advanced.WatermarksTable
}

// The set of column types for which we need to specify `COLLATE "C"` to get
// proper ordering and comparison. Represented as a map[string]bool so that it can be
// combined with the "is the column typename a string" check into one if statement.
var columnBinaryKeyComparison = map[string]bool{
	"varchar": true,
	"bpchar":  true,
	"text":    true,
}

// Construct a filter expression for the backfill query which excludes rows based on their xmin.
// Includes a random aspect so that we receive a trickle of data even while processing parts of
// the table which are fully excluded. When XID filtering is not in use the resulting expression
// is empty since we don't need any of that.
func (db *postgresDatabase) backfillQueryFilterXMIN() string {
	if db.config.Advanced.MinimumBackfillXID != "" {
		const filterValidXID = "(xmin::text::bigint >= 3)"
		var filterLowerXID = fmt.Sprintf("((((xmin::text::bigint - %s::bigint)<<32)>>32) >= 0)", db.config.Advanced.MinimumBackfillXID)

		// Note: We use XMAX here because it's the largest of the "current XID" values and we never want to
		// exclude anything except for rows whose xmin values are from earlier wraparound epochs.
		const filterUpperXID = "((((txid_snapshot_xmax(txid_current_snapshot()) - xmin::text::bigint)<<32)>>32) >= 0)"

		// Final query expression:
		//   - Select 0.1% of rows at random to ensure we have a trickle of data movement even in fully excluded parts of the table.
		//   - Exclude rows with invalid XIDs. This may not actually be necessary as PostgreSQL should never send those, but it can't hurt.
		//   - Exclude rows whose XMIN value is less than the configured minimum.
		//   - Exclude rows whose XMIN valid is _greater_ than the current server XID, as they're from earlier wraparound epochs.
		return fmt.Sprintf("((RANDOM() < 0.001) OR (%s AND %s AND %s))", filterValidXID, filterLowerXID, filterUpperXID)
	}
	return ""
}

func (db *postgresDatabase) keylessScanQuery(_ *sqlcapture.DiscoveryInfo, schemaName, tableName string) string {
	var query = new(strings.Builder)

	// Include xmin when using XID filtering
	var xidFiltered = db.config.Advanced.MinimumBackfillXID != ""
	if xidFiltered {
		fmt.Fprintf(query, `SELECT ctid, xmin::text AS xmin, * FROM "%s"."%s"`, schemaName, tableName)
	} else {
		fmt.Fprintf(query, `SELECT ctid, * FROM "%s"."%s"`, schemaName, tableName)
	}

	fmt.Fprintf(query, ` WHERE ctid > $1`)
	if xidFiltered {
		fmt.Fprintf(query, ` AND %s`, db.backfillQueryFilterXMIN())
	}
	fmt.Fprintf(query, ` LIMIT %d;`, db.config.Advanced.BackfillChunkSize)
	return query.String()
}

func (db *postgresDatabase) buildScanQuery(start, isPrecise bool, keyColumns []string, columnTypes map[string]interface{}, schemaName, tableName string) string {
	// Construct lists of key specifiers and placeholders. They will be joined with commas and used in the query itself.
	var pkey []string
	var args []string
	for idx, colName := range keyColumns {
		var quotedName = quoteColumnName(colName)
		// If a precise backfill is requested *and* the column type requires binary ordering for precise
		// backfill comparisons to work, add the 'COLLATE "C"' qualifier to the column name.
		if colType, ok := columnTypes[colName].(string); ok && isPrecise && columnBinaryKeyComparison[colType] {
			pkey = append(pkey, quotedName+` COLLATE "C"`)
		} else {
			pkey = append(pkey, quotedName)
		}
		args = append(args, fmt.Sprintf("$%d", idx+1))
	}

	// Generate a list of individual WHERE clauses which should be ANDed together
	var whereClauses []string
	if !start {
		whereClauses = append(whereClauses, fmt.Sprintf(`(%s) > (%s)`, strings.Join(pkey, ", "), strings.Join(args, ", ")))
	}

	var xidFiltered = db.config.Advanced.MinimumBackfillXID != ""
	if xidFiltered {
		whereClauses = append(whereClauses, db.backfillQueryFilterXMIN())
	}

	// Construct the query itself
	var query = new(strings.Builder)
	if xidFiltered {
		fmt.Fprintf(query, `SELECT xmin::text AS xmin, * FROM "%s"."%s"`, schemaName, tableName)
	} else {
		fmt.Fprintf(query, `SELECT * FROM "%s"."%s"`, schemaName, tableName)
	}

	if len(whereClauses) > 0 {
		fmt.Fprintf(query, " WHERE %s", strings.Join(whereClauses, " AND "))
	}
	fmt.Fprintf(query, ` ORDER BY %s`, strings.Join(pkey, ", "))
	fmt.Fprintf(query, " LIMIT %d;", db.config.Advanced.BackfillChunkSize)
	return query.String()
}

func quoteColumnName(name string) string {
	// From https://www.postgresql.org/docs/14/sql-syntax-lexical.html#SQL-SYNTAX-IDENTIFIERS:
	//
	//     Quoted identifiers can contain any character, except the character with code zero.
	//     (To include a double quote, write two double quotes.)
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (db *postgresDatabase) explainQuery(ctx context.Context, streamID, query string, args []interface{}) {
	// Only EXPLAIN the backfill query once per connector invocation
	if db.explained == nil {
		db.explained = make(map[sqlcapture.StreamID]struct{})
	}
	if _, ok := db.explained[streamID]; ok {
		return
	}
	db.explained[streamID] = struct{}{}

	// Ask the database to EXPLAIN the backfill query
	var explainQuery = "EXPLAIN " + query
	logrus.WithFields(logrus.Fields{
		"id":    streamID,
		"query": explainQuery,
	}).Info("explain backfill query")
	explainResult, err := db.conn.Query(ctx, explainQuery, args...)
	if err != nil {
		logrus.WithField("id", streamID).Error("unable to execute query")
		return
	}
	defer explainResult.Close()

	// Log the response, doing a bit of extra work to make it readable
	var keys = explainResult.FieldDescriptions()
	for explainResult.Next() {
		var vals, err = explainResult.Values()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"id":  streamID,
				"err": err,
			}).Error("error getting row value")
			return
		}

		var result []string
		for idx, val := range vals {
			result = append(result, fmt.Sprintf("%s=%v", string(keys[idx].Name), val))
		}
		logrus.WithFields(logrus.Fields{
			"id":       streamID,
			"response": result,
		}).Info("explain backfill query")
	}
}

// decodeRowValues decodes the raw values from a pgx.Rows object into a slice of Go values.
func decodeRowValues(rows pgx.Rows) ([]any, error) {
	var fds = rows.FieldDescriptions()
	var typeMap = rows.Conn().TypeMap()
	var values = make([]any, 0, len(fds))
	var rawValues = rows.RawValues()

	for i := range fds {
		var buf = rawValues[i]
		var fd = &fds[i]

		if buf == nil {
			values = append(values, nil)
			continue
		}

		if dt, ok := typeMap.TypeForOID(fd.DataTypeOID); ok {
			value, err := dt.Codec.DecodeValue(typeMap, fd.DataTypeOID, fd.Format, buf)
			if _, ok := err.(*time.ParseError); ok && fd.DataTypeOID == pgtype.TimestamptzOID {
				// PostgreSQL supports dates/timestamps with years greater than 9999 or less than
				// 0 AD, but the PGX client library uses a naive `time.Parse()` call which doesn't
				// support 5-digit or negative years. We can't represent those as RFC3339 timestamps
				// anyway, so if a timestamp fails to parse just map it to a sentinel value.
				value = negativeInfinityTimestamp
			} else if err != nil {
				return nil, fmt.Errorf("error decoding value for column %q: %w", string(fd.Name), err)
			}
			values = append(values, value)
		} else {
			switch fd.Format {
			case pgtype.TextFormatCode:
				values = append(values, string(buf))
			case pgtype.BinaryFormatCode:
				newBuf := make([]byte, len(buf))
				copy(newBuf, buf)
				values = append(values, newBuf)
			default:
				return nil, fmt.Errorf("unknown format code %d", fd.Format)
			}
		}
	}

	return values, nil
}

func compareXID32(a, b uint32) int {
	// Compare two XIDs according to circular comparison logic such that
	// a < b if B lies in the range [a, a+2^31).
	if a == b {
		return 0 // a == b
	} else if ((b - a) & 0x80000000) == 0 {
		return -1 // a < b
	}
	return 1 // a > b
}
