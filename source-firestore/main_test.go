package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	firestore "cloud.google.com/go/firestore"
	"github.com/bradleyjkemp/cupaloy"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	st "github.com/estuary/connectors/source-boilerplate/testing"
	pc "github.com/estuary/flow/go/protocols/capture"
	"github.com/estuary/flow/go/protocols/flow"
	pf "github.com/estuary/flow/go/protocols/flow"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

var testCredentialsPath = flag.String(
	"creds_path",
	"~/.config/gcloud/application_default_credentials.json",
	"Path to the credentials JSON to use for test authentication",
)
var testProjectID = flag.String(
	"project_id",
	"estuary-sandbox",
	"The project ID to interact with during automated tests",
)
var testDatabaseName = flag.String(
	"database",
	"(default)",
	"The database to interact with during automated tests",
)

// Most capture tests run two capture phases in order to verify that
// resuming from a prior checkpoint works correctly. These are the
// sentinel values used to shut down those captures.
const (
	restartSentinel  = "5f91dae7-dc4d-48d9-bfec-2d3bfeec4164"
	shutdownSentinel = "a6d1c2e4-be25-4415-8f03-ab20abbcc5a6"
)

var DefaultSanitizers = make(map[string]*regexp.Regexp)

func TestMain(m *testing.M) {
	flag.Parse()
	if level, err := log.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil {
		log.SetLevel(level)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.JSONFormatter{
		DataKey:  "data",
		FieldMap: log.FieldMap{log.FieldKeyTime: "@ts"},
	})
	DefaultSanitizers[`"<TIMESTAMP>"`] = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]+:[0-9]+)"`)
	DefaultSanitizers["project-id-123456"] = regexp.MustCompile(regexp.QuoteMeta(*testProjectID))
	os.Exit(m.Run())
}

func TestSpec(t *testing.T) {
	driver := driver{}
	response, err := driver.Spec(context.Background(), &pc.Request_Spec{})
	require.NoError(t, err)

	formatted, err := json.MarshalIndent(response, "", "  ")
	require.NoError(t, err)
	cupaloy.SnapshotT(t, string(formatted))
}

func TestSimpleCapture(t *testing.T) {
	var ctx = testContext(t, 10*time.Second)
	var capture = simpleCapture(t, "docs")
	var client = testFirestoreClient(ctx, t)
	t.Run("one", func(t *testing.T) {
		client.Upsert(ctx, t,
			"docs/1", `{"data": 1}`,
			"docs/2", `{"data": 2}`,
			"docs/3", `{"data": 3}`,
			"docs/4", `{"data": 4}`,
			"docs/5", `{"data": 5}`,
			"docs/6", `{"data": 6}`,
		)
		verifyCapture(ctx, t, capture)
	})
	t.Run("two", func(t *testing.T) {
		client.Upsert(ctx, t,
			"docs/7", `{"data": 7}`,
			"docs/8", `{"data": 8}`,
			"docs/9", `{"data": 9}`,
			"docs/10", `{"data": 10}`,
			"docs/11", `{"data": 11}`,
			"docs/12", `{"data": 12}`,
		)
		verifyCapture(ctx, t, capture)
	})
}

func TestDeletions(t *testing.T) {
	// TODO(wgd): We should investigate this further at some point. I have
	// trouble believing that deletion events straight-up Don't Work, but
	// I have poked at this for more than an entire work day at this point
	// and as far as I can tell we're doing things correctly and just not
	// ever being informed about deletions.
	//
	// (Just to be clear since this was an obvious thing to check: It makes
	// no difference whether we use read times like we currently do, or the
	// opaque resume tokens supplied by the server. Either way we just never
	// seem to receive any deletion events.)
	//
	// We should revisit this in the future, ideally once we have support for
	// collection level 'Truncated' and 'Backfill Complete' signals. Because if
	// we can't get deletions working reliably in the simplest possible test
	// here, we should probably consider removing deletion handling from the
	// change streaming path entirely and instead rely on something like a
	// "re-backfill collection every <period>" feature to ensure that deletions
	// are captured eventually.
	t.Skip("Firestore change streaming is unreliable for document deletion")

	var ctx = testContext(t, 10*time.Second)
	var capture = simpleCapture(t, "docs")
	var client = testFirestoreClient(ctx, t)
	t.Run("one", func(t *testing.T) {
		client.Upsert(ctx, t,
			"docs/1", `{"data": 1}`,
			"docs/2", `{"data": 2}`,
			"docs/3", `{"data": 3}`,
			"docs/4", `{"data": 4}`,
		)
		time.Sleep(1 * time.Second)
		client.Delete(ctx, t, "docs/1", "docs/2")
		verifyCapture(ctx, t, capture)
	})
	t.Run("two", func(t *testing.T) {
		client.Delete(ctx, t, "docs/3", "docs/4")
		verifyCapture(ctx, t, capture)
	})
}

// This test exercises some behaviors around adding a new capture binding,
// especially in the tricky edge case where the new binding maps to the
// same collection group ID as a preexisting one.
func TestAddedBindingSameGroup(t *testing.T) {
	var ctx = testContext(t, 30*time.Second)
	var capture = simpleCapture(t, "users/*/docs")
	var client = testFirestoreClient(ctx, t)
	t.Run("one", func(t *testing.T) {
		client.Upsert(ctx, t,
			"users/1/docs/1", `{"data": 1}`,
			"users/1/docs/2", `{"data": 2}`,
			"groups/3/docs/7", `{"data": 7}`,
			"groups/3/docs/8", `{"data": 8}`,
		)
		verifyCapture(ctx, t, capture)
	})
	capture.Bindings = append(capture.Bindings, simpleBindings("groups/*/docs")...)
	t.Run("two", func(t *testing.T) {
		client.Upsert(ctx, t,
			"users/1/docs/3", `{"data": 3}`,
			"users/1/docs/4", `{"data": 4}`,
			"groups/2/docs/5", `{"data": 5}`,
			"groups/2/docs/6", `{"data": 6}`,
		)
		verifyCapture(ctx, t, capture)
	})
}

func TestManySmallWrites(t *testing.T) {
	var ctx = testContext(t, 40*time.Second)
	var capture = simpleCapture(t, "users/*/docs")
	var client = testFirestoreClient(ctx, t)

	for user := 0; user < 5; user++ {
		for doc := 0; doc < 5; doc++ {
			var docName = fmt.Sprintf("users/%d/docs/%d", user, doc)
			var docData = fmt.Sprintf(`{"user": %d, "doc": %d}`, user, doc)
			client.Upsert(ctx, t, docName, docData)
			time.Sleep(100 * time.Millisecond)
		}
	}
	time.Sleep(2 * time.Second)
	t.Run("one", func(t *testing.T) { verifyCapture(ctx, t, capture) })

	for user := 5; user < 10; user++ {
		for doc := 0; doc < 5; doc++ {
			var docName = fmt.Sprintf("users/%d/docs/%d", user, doc)
			var docData = fmt.Sprintf(`{"user": %d, "doc": %d}`, user, doc)
			client.Upsert(ctx, t, docName, docData)
			time.Sleep(100 * time.Millisecond)
		}
	}
	time.Sleep(2 * time.Second)
	t.Run("two", func(t *testing.T) { verifyCapture(ctx, t, capture) })
}

func TestMultipleWatches(t *testing.T) {
	var ctx = testContext(t, 40*time.Second)
	var capture = simpleCapture(t, "users/*/docs", "users/*/notes", "users/*/tasks")
	var client = testFirestoreClient(ctx, t)

	for user := 0; user < 5; user++ {
		for item := 0; item < 5; item++ {
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/docs/%d`, user, item), `{"data": "placeholder"}`)
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/notes/%d`, user, item), `{"data": "placeholder"}`)
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/tasks/%d`, user, item), `{"data": "placeholder"}`)
		}
	}
	time.Sleep(2 * time.Second)
	t.Run("one", func(t *testing.T) { verifyCapture(ctx, t, capture) })

	for user := 5; user < 10; user++ {
		for item := 0; item < 5; item++ {
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/docs/%d`, user, item), `{"data": "placeholder"}`)
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/notes/%d`, user, item), `{"data": "placeholder"}`)
			client.Upsert(ctx, t, fmt.Sprintf(`users/%d/tasks/%d`, user, item), `{"data": "placeholder"}`)
		}
	}
	time.Sleep(2 * time.Second)
	t.Run("two", func(t *testing.T) { verifyCapture(ctx, t, capture) })
}

func TestBindingDeletion(t *testing.T) {
	var ctx = testContext(t, 20*time.Second)
	var client = testFirestoreClient(ctx, t)
	for idx := 0; idx < 20; idx++ {
		client.Upsert(ctx, t, fmt.Sprintf("docs/%d", idx), `{"data": "placeholder"}`)
	}
	time.Sleep(2 * time.Second)

	var capture = simpleCapture(t, "docs")
	t.Run("one", func(t *testing.T) { verifyCapture(ctx, t, capture) })
	capture.Bindings = simpleBindings("other")
	t.Run("two", func(t *testing.T) { verifyCapture(ctx, t, capture) })
	capture.Bindings = simpleBindings("docs")
	t.Run("three", func(t *testing.T) { verifyCapture(ctx, t, capture) })
}

func TestDiscovery(t *testing.T) {
	var ctx = testContext(t, 300*time.Second)
	var client = testFirestoreClient(ctx, t)
	client.Upsert(ctx, t, "users/1", `{"name": "Will"}`)
	client.Upsert(ctx, t, "users/2", `{"name": "Alice"}`)
	client.Upsert(ctx, t, "users/1/docs/1", `{"foo": "bar", "asdf": 123}`)
	client.Upsert(ctx, t, "users/1/docs/2", `{"foo": "bar", "asdf": 456}`)
	client.Upsert(ctx, t, "users/2/docs/3", `{"foo": "baz", "asdf": 789}`)
	client.Upsert(ctx, t, "users/2/docs/4", `{"foo": "baz", "asdf": 1000}`)
	time.Sleep(1 * time.Second)
	var cs = simpleCapture(t)
	cs.EndpointSpec.(*config).Advanced.ExtraCollections = []string{"flow_source_tests/*/nonexistent/*/extra/*/collection"}
	cs.VerifyDiscover(ctx, t, regexp.MustCompile(regexp.QuoteMeta("flow_source_tests")))
}

func TestNestedCollections(t *testing.T) {
	var ctx = testContext(t, 300*time.Second)
	var client = testFirestoreClient(ctx, t)
	var capture = simpleCapture(t, "nested_users", "nested_users/*/docs")

	client.Upsert(ctx, t, "nested_users/B1", `{"name": "Alice"}`)
	client.Upsert(ctx, t, "nested_users/B2", `{"name": "Bob"}`)
	client.Upsert(ctx, t, "nested_users/B1/docs/1", `{"foo": "bar", "asdf": 123}`)
	client.Upsert(ctx, t, "nested_users/B1/docs/2", `{"foo": "bar", "asdf": 456}`)
	client.Upsert(ctx, t, "nested_users/B2/docs/3", `{"foo": "baz", "asdf": 789}`)
	client.Upsert(ctx, t, "nested_users/B2/docs/4", `{"foo": "baz", "asdf": 1000}`)
	t.Run("init", func(t *testing.T) { verifyCapture(ctx, t, capture) })

	client.Upsert(ctx, t, "nested_users/R3", `{"name": "Carol"}`)
	client.Upsert(ctx, t, "nested_users/R4", `{"name": "Dave"}`)
	client.Upsert(ctx, t, "nested_users/R3/docs/5", `{"foo": "bar", "asdf": 123}`)
	client.Upsert(ctx, t, "nested_users/R3/docs/6", `{"foo": "bar", "asdf": 456}`)
	client.Upsert(ctx, t, "nested_users/R4/docs/7", `{"foo": "baz", "asdf": 789}`)
	client.Upsert(ctx, t, "nested_users/R4/docs/8", `{"foo": "baz", "asdf": 1000}`)
	t.Run("repl", func(t *testing.T) { verifyCapture(ctx, t, capture) })
}

func TestRestartCursorUpdates(t *testing.T) {
	var ctx = testContext(t, 120*time.Second)
	var capture = simpleCapture(t, "users/*/docs")
	var client = testFirestoreClient(ctx, t)

	var res resource
	require.NoError(t, json.Unmarshal(capture.Bindings[0].ResourceConfigJson, &res))
	res.RestartCursorPath = "/monotonic_id"
	var resJSON, err = json.Marshal(res)
	require.NoError(t, err)
	capture.Bindings[0].ResourceConfigJson = resJSON

	restartCursorPendingInterval = 1 * time.Millisecond

	var timestampBase = time.Date(2024, 05, 28, 00, 00, 00, 00, time.UTC)
	var root = client.Conn.Collection("flow_source_tests").Doc(strings.ReplaceAll(t.Name(), "/", "_"))
	for user := 1; user < 3; user++ {
		for doc := 0; doc < 10; doc++ {
			root.Collection("users").Doc(strconv.Itoa(user)).Collection("docs").Doc(strconv.Itoa(doc)).Create(ctx, map[string]any{
				"monotonic_id": timestampBase.Add(time.Duration(user*10+doc) * time.Minute),
				"user":         user,
				"doc":          doc,
			})
		}
	}
	time.Sleep(3 * time.Second)
	t.Run("init", func(t *testing.T) { verifyCapture(ctx, t, capture) })

	for user := 3; user < 10; user++ {
		for doc := 0; doc < 10; doc++ {
			root.Collection("users").Doc(strconv.Itoa(user)).Collection("docs").Doc(strconv.Itoa(doc)).Create(ctx, map[string]any{
				"monotonic_id": timestampBase.Add(time.Duration(user*10+doc) * time.Minute),
				"user":         user,
				"doc":          doc,
			})
		}
	}
	time.Sleep(3 * time.Second)
	t.Run("repl", func(t *testing.T) { verifyCapture(ctx, t, capture) })

	var checkpoint captureState
	require.NoError(t, json.Unmarshal(capture.Checkpoint, &checkpoint))
	for _, resourceState := range checkpoint.Resources {
		resourceState.Inconsistent = true
		resourceState.RestartCursorValue = "2024-05-28T00:49:00Z"
		resourceState.Backfill.StartAfter = time.Now().Add(100 * time.Millisecond)
	}
	checkpointJSON, err := json.Marshal(&checkpoint)
	require.NoError(t, err)
	capture.Checkpoint = checkpointJSON

	t.Run("restart", func(t *testing.T) { verifyCapture(ctx, t, capture) })
}

func testContext(t testing.TB, duration time.Duration) context.Context {
	t.Helper()
	if testing.Short() && duration > 10*time.Second {
		t.Skip("skipping long test")
	}
	var ctx, cancel = context.WithTimeout(context.Background(), duration)
	t.Cleanup(cancel)
	return ctx
}

func simpleCapture(t testing.TB, names ...string) *st.CaptureSpec {
	t.Helper()
	if os.Getenv("TEST_DATABASE") != "yes" {
		t.Skipf("skipping %q capture: ${TEST_DATABASE} != \"yes\"", t.Name())
	}

	// Load credentials from disk and construct an endpoint spec
	var credentialsPath = strings.ReplaceAll(*testCredentialsPath, "~", os.Getenv("HOME"))
	credentialsJSON, err := ioutil.ReadFile(credentialsPath)
	require.NoError(t, err)

	var endpointSpec = &config{
		CredentialsJSON: string(credentialsJSON),
		DatabasePath:    fmt.Sprintf("projects/%s/databases/%s", *testProjectID, *testDatabaseName),
	}

	return &st.CaptureSpec{
		Driver:       new(driver),
		EndpointSpec: endpointSpec,
		Bindings:     simpleBindings(names...),
		Validator:    &st.SortedCaptureValidator{},
		Sanitizers:   DefaultSanitizers,
	}
}

func simpleBindings(names ...string) []*flow.CaptureSpec_Binding {
	var bindings []*flow.CaptureSpec_Binding
	for _, name := range names {
		var path = "flow_source_tests/*/" + name
		bindings = append(bindings, &flow.CaptureSpec_Binding{
			Collection:         flow.CollectionSpec{Name: flow.Collection(path)},
			ResourceConfigJson: json.RawMessage(fmt.Sprintf(`{"path": %q, "backfillMode": "async"}`, path)),
			ResourcePath:       []string{path},
			StateKey:           url.QueryEscape(path),
		})
	}
	return bindings
}

// verifyCapture performs a capture using the provided st.CaptureSpec and shuts it down after
// a suitable time has elapsed without any documents or state checkpoints being emitted. It
// then performs snapshot verification on the results.
func verifyCapture(ctx context.Context, t testing.TB, cs *st.CaptureSpec) {
	t.Helper()
	var captureCtx, cancelCapture = context.WithCancel(ctx)
	const shutdownDelay = 2000 * time.Millisecond
	var shutdownWatchdog *time.Timer
	cs.Capture(captureCtx, t, func(data json.RawMessage) {
		if shutdownWatchdog == nil {
			shutdownWatchdog = time.AfterFunc(shutdownDelay, func() {
				log.WithField("delay", shutdownDelay.String()).Debug("capture shutdown watchdog expired")
				cancelCapture()
			})
		}
		shutdownWatchdog.Reset(shutdownDelay)
	})
	cupaloy.SnapshotT(t, cs.Summary())
	cs.Reset()
}

type firestoreClient struct {
	Conn   *firestore.Client
	Prefix string
}

func testFirestoreClient(ctx context.Context, t testing.TB) *firestoreClient {
	t.Helper()
	if os.Getenv("TEST_DATABASE") != "yes" {
		t.Skipf("skipping %q capture: ${TEST_DATABASE} != \"yes\"", t.Name())
	}

	var credentialsPath = strings.ReplaceAll(*testCredentialsPath, "~", os.Getenv("HOME"))
	credentialsJSON, err := ioutil.ReadFile(credentialsPath)
	require.NoError(t, err)

	client, err := firestore.NewClient(ctx, *testProjectID, option.WithCredentialsJSON(credentialsJSON))
	require.NoError(t, err)

	var prefix = fmt.Sprintf("flow_source_tests/%s", strings.ReplaceAll(t.Name(), "/", "_"))
	var tfc = &firestoreClient{
		Conn:   client,
		Prefix: prefix + "/",
	}
	t.Cleanup(func() {
		tfc.deleteCollectionRecursive(context.Background(), t, tfc.Conn.Collection("flow_source_tests"))
		tfc.Conn.Close()
	})
	tfc.deleteCollectionRecursive(ctx, t, tfc.Conn.Collection("flow_source_tests"))

	_, err = client.Batch().Set(client.Doc(prefix), map[string]interface{}{"testName": t.Name()}).Commit(ctx)
	require.NoError(t, err)

	return tfc
}

func (c *firestoreClient) DeleteCollection(ctx context.Context, t testing.TB, collection string) {
	t.Helper()
	c.deleteCollectionRecursive(ctx, t, c.Conn.Collection(c.Prefix+collection))
}

func (c *firestoreClient) deleteCollectionRecursive(ctx context.Context, t testing.TB, ref *firestore.CollectionRef) {
	t.Helper()
	docs, err := ref.DocumentRefs(ctx).GetAll()
	require.NoError(t, err)
	for _, doc := range docs {
		log.WithField("path", doc.Path).Trace("deleting doc")
		subcolls, err := doc.Collections(ctx).GetAll()
		require.NoError(t, err)
		for _, subcoll := range subcolls {
			c.deleteCollectionRecursive(ctx, t, subcoll)
		}
		_, err = doc.Delete(ctx)
		require.NoError(t, err)
	}
}

func (c *firestoreClient) Upsert(ctx context.Context, t testing.TB, docKVs ...string) {
	t.Helper()

	// Interpret the variadic part of the arguments as an alternating sequence
	// of document names and document values.
	var names []string
	var values []string
	for idx, str := range docKVs {
		if idx%2 == 0 {
			names = append(names, str)
		} else {
			values = append(values, str)
		}
	}
	log.WithField("count", len(names)).Trace("upserting test documents")

	var wb = c.Conn.Batch()
	for idx := range values {
		var fields = make(map[string]interface{})
		var err = json.Unmarshal(json.RawMessage(values[idx]), &fields)
		require.NoError(t, err)
		wb = wb.Set(c.Conn.Doc(c.Prefix+names[idx]), fields, firestore.MergeAll)
	}
	var _, err = wb.Commit(ctx)
	require.NoError(t, err)
}

func (c *firestoreClient) Delete(ctx context.Context, t testing.TB, names ...string) {
	t.Helper()
	log.WithField("count", len(names)).Debug("deleting test documents")

	var wb = c.Conn.Batch()
	for _, docName := range names {
		wb = wb.Delete(c.Conn.Doc(c.Prefix + docName))
	}
	var _, err = wb.Commit(ctx)
	require.NoError(t, err)
}

func TestDocumentReferences(t *testing.T) {
	var ctx = testContext(t, 10*time.Second)
	var capture = simpleCapture(t, "docs")
	var client = testFirestoreClient(ctx, t)
	t.Run("backfill", func(t *testing.T) {
		client.Conn.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			var docA = client.Conn.Doc(client.Prefix + "docs/1")
			var docB = client.Conn.Doc(client.Prefix + "docs/2")
			tx.Set(docA, map[string]any{"data": 1}, firestore.MergeAll)
			tx.Set(docB, map[string]any{"ref": docA}, firestore.MergeAll)
			return nil
		})
		verifyCapture(ctx, t, capture)
	})
	t.Run("replication", func(t *testing.T) {
		client.Conn.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
			var docA = client.Conn.Doc(client.Prefix + "docs/3")
			var docB = client.Conn.Doc(client.Prefix + "docs/4")
			tx.Set(docA, map[string]any{"data": 3}, firestore.MergeAll)
			tx.Set(docB, map[string]any{"ref": docA}, firestore.MergeAll)
			return nil
		})
		verifyCapture(ctx, t, capture)
	})
}

func TestInitResourceStates(t *testing.T) {
	var testNow = time.Date(2024, 5, 30, 12, 0, 0, 0, time.UTC)

	// Helper to construct a binding with specific path and backfill mode.
	var createBinding = func(path string, mode backfillMode, restartCursorPath string) *pf.CaptureSpec_Binding {
		resourceConfig := resource{
			Path:              path,
			BackfillMode:      mode,
			RestartCursorPath: restartCursorPath,
		}
		configJSON, _ := json.Marshal(resourceConfig)
		return &pf.CaptureSpec_Binding{
			StateKey:           url.QueryEscape(path),
			ResourceConfigJson: configJSON,
		}
	}

	var tcs = []struct {
		name     string
		states   map[boilerplate.StateKey]*resourceState
		bindings []*pf.CaptureSpec_Binding
	}{
		{
			name:   "new binding with no previous state",
			states: map[boilerplate.StateKey]*resourceState{},
			bindings: []*pf.CaptureSpec_Binding{
				createBinding("users/*/docs", "async", ""),
			},
		},
		{
			name: "still consistent with ongoing backfill should continue backfill",
			states: map[boilerplate.StateKey]*resourceState{
				"users%2F%2A%2Fdocs": {
					ReadTime: testNow.Add(-1 * time.Hour),
					Backfill: &backfillState{
						StartAfter: testNow.Add(-30 * time.Minute),
						Cursor:     "users/123/docs/456",
						MTime:      testNow.Add(-45 * time.Minute),
					},
				},
			},
			bindings: []*pf.CaptureSpec_Binding{
				createBinding("users/*/docs", "async", ""),
			},
		},
		{
			name: "still consistent with completed backfill should preserve that",
			states: map[boilerplate.StateKey]*resourceState{
				"users%2F%2A%2Fdocs": {
					ReadTime: testNow.Add(-1 * time.Hour),
					Backfill: &backfillState{
						StartAfter: testNow.Add(-2 * time.Hour),
						Completed:  true,
					},
				},
			},
			bindings: []*pf.CaptureSpec_Binding{
				createBinding("users/*/docs", "async", ""),
			},
		},
		{
			name: "inconsistent state with ongoing backfill should continue backfill",
			states: map[boilerplate.StateKey]*resourceState{
				"users%2F%2A%2Fdocs": {
					ReadTime:     testNow.Add(-1 * time.Hour),
					Inconsistent: true,
					Backfill: &backfillState{
						StartAfter: testNow.Add(-30 * time.Minute),
						Cursor:     "users/123/docs/456",
						MTime:      testNow.Add(-45 * time.Minute),
					},
				},
			},
			bindings: []*pf.CaptureSpec_Binding{
				createBinding("users/*/docs", "async", ""),
			},
		},
		{
			name: "inconsistent state with completed backfill should schedule new backfill",
			states: map[boilerplate.StateKey]*resourceState{
				"users%2F%2A%2Fdocs": {
					ReadTime:     testNow.Add(-1 * time.Hour),
					Inconsistent: true,
					Backfill: &backfillState{
						StartAfter: testNow.Add(-2 * time.Hour),
						Completed:  true,
					},
				},
			},
			bindings: []*pf.CaptureSpec_Binding{
				createBinding("users/*/docs", "async", ""),
			},
		},
	}

	var out = new(strings.Builder)
	for i, tc := range tcs {
		if i > 0 {
			fmt.Fprintf(out, "\n")
		}
		fmt.Fprintf(out, "--- %s ---\n", tc.name)
		var outputStates, err = initResourceStates(&config{}, tc.states, tc.bindings, testNow)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			continue
		}
		for stateKey, state := range outputStates {
			var bs, err = json.MarshalIndent(state, "", "  ")
			require.NoError(t, err)
			fmt.Fprintf(out, "%s:\n%s\n", stateKey, string(bs))
		}
	}
	cupaloy.SnapshotT(t, out.String())
}
