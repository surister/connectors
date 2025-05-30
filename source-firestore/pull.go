package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	firestore "cloud.google.com/go/firestore"
	firestore_v1 "cloud.google.com/go/firestore/apiv1"
	firebase "firebase.google.com/go"
	boilerplate "github.com/estuary/connectors/source-boilerplate"
	pc "github.com/estuary/flow/go/protocols/capture"
	pf "github.com/estuary/flow/go/protocols/flow"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	firestore_pb "google.golang.org/genproto/googleapis/firestore/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var firebaseScopes = []string{
	"https://www.googleapis.com/auth/datastore",
}

const watchTargetID = 1245678

// Non-permanent failures like Unavailable or ResourceExhausted can be retried
// after a little while.
const retryInterval = 60 * time.Second

// Log progress messages after every N documents on a particular stream
const progressLogInterval = 10000

// A new backfill of a collection may only occur after at least this much time has
// elapsed since the previous backfill was started.
const (
	backfillRestartDelayNoRestartCursor   = 24 * time.Hour
	backfillRestartDelayWithRestartCursor = 5 * time.Minute
)

const (
	backfillChunkSize   = 256
	concurrentBackfills = 2
)

// The minimum length of time for which a new restart cursor value will be buffered in
// memory before being committed to the state checkpoint. This ensures that in the event
// of a re-backfill there will be some minimum amount of overlap between the most recent
// change events and the restart point of the backfill.
//
// This is a constant in typical usage, but is made a variable so that tests can override
// the value to something smaller.
var restartCursorPendingInterval = 5 * time.Minute

func (driver) Pull(open *pc.Request_Open, stream *boilerplate.PullOutput) error {
	log.Debug("connector started")

	var cfg config
	if err := pf.UnmarshalStrict(open.Capture.ConfigJson, &cfg); err != nil {
		return fmt.Errorf("parsing endpoint config: %w", err)
	}

	var prevState captureState
	if open.StateJson != nil {
		if err := pf.UnmarshalStrict(open.StateJson, &prevState); err != nil {
			return fmt.Errorf("parsing state checkpoint: %w", err)
		}
	}

	updatedResourceStates, err := initResourceStates(&cfg, prevState.Resources, open.Capture.Bindings, time.Now())
	if err != nil {
		return fmt.Errorf("error initializing resource states: %w", err)
	}

	// Build a mapping of document paths to state keys, to allow efficient lookups of state keys
	// from the path of retrieved documents.
	stateKeys := make(map[string]boilerplate.StateKey)
	for sk, res := range updatedResourceStates {
		stateKeys[res.path] = sk
	}

	var capture = &capture{
		Config: cfg,
		State: &captureState{
			Resources: updatedResourceStates,
			stateKeys: stateKeys,
		},
		Output: stream,

		backfillSemaphore: semaphore.NewWeighted(concurrentBackfills),
		streamsInCatchup:  new(sync.WaitGroup),
	}
	return capture.Run(stream.Context())
}

type capture struct {
	Config config
	State  *captureState
	Output *boilerplate.PullOutput

	backfillSemaphore *semaphore.Weighted
	streamsInCatchup  *sync.WaitGroup
}

type captureState struct {
	sync.RWMutex
	Resources map[boilerplate.StateKey]*resourceState `json:"bindingStateV1,omitempty"`
	stateKeys map[string]boilerplate.StateKey         // Allow for lookups of the stateKey from a document path.
}

type resourceState struct {
	ReadTime time.Time
	Backfill *backfillState

	// The 'Inconsistent' flag is set when catchup failure forces the connector
	// to "skip ahead" to the latest changes for some collection(s), and indicates
	// that at some point in the future a new backfill of that collection(s) needs
	// to be performed to re-establish consistency.
	Inconsistent bool `json:"Inconsistent,omitempty"`

	// The `RestartCursorValue` property is updated periodically during change streaming
	// (if the binding has a `RestartCursorPath` setting) so it holds an arbitrarily selected
	// restart-cursor value which was recently observed in the dataset.
	RestartCursorValue any `json:"RestartCursorValue,omitempty"`

	restartCursorPath           []string  // The parsed path to the restart-cursor property. Not serialized as part of the state, it's just copied from the resource spec for convenience.
	pendingRestartCursorTimeout time.Time // The timestamp after which the pending restart-cursor value may be promoted to active. Zero if there is no pending value.
	pendingRestartCursorValue   any       // A new restart-cursor value which has been buffered temporarily and may eventually be promoted to active.

	bindingIndex int
	path         string
}

type backfillState struct {
	// The time after which a backfill of this resource may begin processing
	// documents. Used for rate-limiting of retry attempts.
	StartAfter time.Time

	// True if the backfill is completed.
	Completed bool

	Cursor string    // The last document backfilled
	MTime  time.Time // The UpdateTime of that document when we backfilled it
}

func (s *backfillState) Equal(x *backfillState) bool {
	if s == nil {
		return x == nil
	} else if x == nil {
		return false
	} else {
		return s.Cursor == x.Cursor && s.MTime.Equal(x.MTime)
	}
}

func (s *backfillState) String() string {
	return fmt.Sprintf("%s at %s", s.Cursor, s.MTime)
}

// Given the prior resource states from the last DriverCheckpoint along with
// the current capture bindings, compute a new set of resource states.
func initResourceStates(cfg *config, prevStates map[boilerplate.StateKey]*resourceState, bindings []*pf.CaptureSpec_Binding, now time.Time) (map[boilerplate.StateKey]*resourceState, error) {
	var states = make(map[boilerplate.StateKey]*resourceState)
	for idx, binding := range bindings {
		var res resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return nil, fmt.Errorf("parsing resource config: %w", err)
		}
		var stateKey = boilerplate.StateKey(binding.StateKey)

		// Construct a new state and add it to the map. Since it's a pointer we can fill
		// out the rest of the state later in the function.
		var state = &resourceState{
			bindingIndex: idx,
			path:         res.Path,
		}
		if res.RestartCursorPath != "" {
			state.restartCursorPath = parseJSONPointer(res.RestartCursorPath)
		}
		states[stateKey] = state

		// If there is no prior state for the binding, initialize it from scratch.
		var prevState, ok = prevStates[stateKey]
		if !ok {
			switch res.BackfillMode {
			case backfillModeNone:
				state.ReadTime = now
				state.Backfill = nil
			case backfillModeAsync:
				state.ReadTime = now
				state.Backfill = &backfillState{StartAfter: now}
			case backfillModeSync:
				state.ReadTime = time.Time{}
				state.Backfill = nil
			default:
				return nil, fmt.Errorf("invalid backfill mode %q for %q", res.BackfillMode, res.Path)
			}
			if res.InitTimestamp != "" {
				var ts, err = time.Parse(time.RFC3339Nano, res.InitTimestamp)
				if err != nil {
					return nil, fmt.Errorf("invalid initTimestamp value %q: %w", res.InitTimestamp, err)
				}
				state.ReadTime = ts
			}
			continue
		}

		// Always retain the latest restart cursor value.
		state.RestartCursorValue = prevState.RestartCursorValue

		// If we're still consistent, we don't need to mess with anything else.
		if !prevState.Inconsistent {
			state.ReadTime = prevState.ReadTime
			state.Backfill = prevState.Backfill
			continue
		}

		// Since we're already inconsistent we can't make it worse, and resetting the read time
		// here makes it slightly more likely that we'll remain caught up going forward.
		state.ReadTime = now

		// If we're inconsistent but there's an ongoing backfill, we need to let it finish,
		// otherwise we need to initialize a new backfill.
		if prevState.Backfill != nil && !prevState.Backfill.Completed {
			state.Backfill = prevState.Backfill
			state.Inconsistent = true // Still inconsistent for later
		} else if res.BackfillMode == backfillModeAsync {
			state.Backfill = &backfillState{
				StartAfter: computeBackfillStartTime(
					cfg, &res,
					prevState.Backfill, now,
					len(prevState.restartCursorPath) != 0,
				),
			}
		}
	}
	return states, nil
}

func computeBackfillStartTime(cfg *config, res *resource, prevBackfill *backfillState, now time.Time, hasRestartCursor bool) time.Time {
	var startTime time.Time
	if prevBackfill != nil {
		startTime = prevBackfill.StartAfter
	}

	// Minimum backfill interval priority:
	// - Resource Config
	// - Advanced Endpoint Config
	// - Default to 24h if no cursor or 5m if there's a cursor
	var delay time.Duration
	if d, err := time.ParseDuration(res.MinBackfillInterval); err == nil && d > 0 {
		delay = d
	} else if d, err := time.ParseDuration(cfg.Advanced.MinBackfillInterval); err == nil && d > 0 {
		delay = d
	} else if hasRestartCursor {
		delay = backfillRestartDelayWithRestartCursor
	} else {
		delay = backfillRestartDelayNoRestartCursor
	}
	startTime = startTime.Add(delay)

	if startTime.Before(now) {
		startTime = now
	}
	return startTime
}

func (s *captureState) Validate() error {
	return nil
}

func (s *captureState) BindingIndex(resourcePath string) (int, bool) {
	s.RLock()
	defer s.RUnlock()
	if sk, ok := s.stateKeys[resourcePath]; ok {
		if state := s.Resources[sk]; state != nil {
			return state.bindingIndex, true
		}
	}
	// Return MaxInt just to be extra clear that we're not capturing this resource
	return math.MaxInt, false
}

func (s *captureState) ReadTime(resourcePath string) (time.Time, bool) {
	s.RLock()
	defer s.RUnlock()
	if sk, ok := s.stateKeys[resourcePath]; ok {
		if state := s.Resources[sk]; state != nil {
			return state.ReadTime, true
		}
	}
	return time.Time{}, false
}

func (s *captureState) BackfillingAsync(rpath resourcePath) bool {
	s.RLock()
	defer s.RUnlock()
	if sk, ok := s.stateKeys[rpath]; ok {
		if state := s.Resources[sk]; state != nil {
			return state.Backfill != nil
		}
	}
	return false
}

func (s *captureState) UpdateReadTimes(collectionID string, readTime time.Time) (json.RawMessage, error) {
	s.Lock()
	var updated = make(map[boilerplate.StateKey]*resourceState)
	for stateKey, resourceState := range s.Resources {
		if getLastCollectionGroupID(resourceState.path) == collectionID {
			resourceState.ReadTime = readTime
			updated[stateKey] = resourceState
		}
	}
	s.Unlock()

	var checkpointJSON, err = json.Marshal(&captureState{Resources: updated})
	if err != nil {
		return nil, fmt.Errorf("error serializing state checkpoint: %w", err)
	}
	return checkpointJSON, nil
}

func (s *captureState) UpdateBackfillState(resourcePaths []resourcePath, state *backfillState) (json.RawMessage, error) {
	s.Lock()
	var updated = make(map[boilerplate.StateKey]*resourceState)
	for stateKey, resourceState := range s.Resources {
		if slices.Contains(resourcePaths, resourceState.path) {
			resourceState.Backfill = state
			updated[stateKey] = resourceState
		}
	}
	s.Unlock()

	var checkpointJSON, err = json.Marshal(&captureState{Resources: updated})
	if err != nil {
		return nil, fmt.Errorf("error serializing state checkpoint: %w", err)
	}
	return checkpointJSON, nil
}

func (s *captureState) MarkInconsistent(collectionID string) (json.RawMessage, bool, error) {
	var allStreamsHaveRestartCursors = true

	s.Lock()
	var updated = make(map[boilerplate.StateKey]*resourceState)
	for stateKey, resourceState := range s.Resources {
		if getLastCollectionGroupID(resourceState.path) == collectionID {
			resourceState.Inconsistent = true
			updated[stateKey] = resourceState
			if len(resourceState.restartCursorPath) == 0 {
				allStreamsHaveRestartCursors = false
			}
		}
	}
	s.Unlock()

	var checkpointJSON, err = json.Marshal(&captureState{Resources: updated})
	if err != nil {
		return nil, false, fmt.Errorf("error serializing state checkpoint: %w", err)
	}
	return checkpointJSON, allStreamsHaveRestartCursors, nil
}

func (c *capture) Run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)

	// Enumerate the sets of watch streams and async backfills we'll need to perform.
	// In both cases we map resource paths to collection IDs, because that's how the
	// underlying API works (so for instance 'users/*/messages' and 'groups/*/messages'
	// are both 'messages').
	var watchCollections = make(map[collectionGroupID]time.Time)
	var backfills []*backfillDescription
	for _, resourceState := range c.State.Resources {
		var collectionID = getLastCollectionGroupID(resourceState.path)
		if startTime, ok := watchCollections[collectionID]; !ok || resourceState.ReadTime.Before(startTime) {
			watchCollections[collectionID] = resourceState.ReadTime
		}
		if resourceState.Backfill == nil || resourceState.Backfill.Completed {
			// Do nothing when no backfill is required
			continue
		}
		log.WithFields(log.Fields{
			"resource":   resourceState.path,
			"collection": collectionID,
			"startAfter": resourceState.Backfill.StartAfter,
			"cursor":     resourceState.Backfill.Cursor,
		}).Debug("backfill required for binding")

		// Determine if there's already a compatible backfill (one with the same collection
		// group ID and backfill cursor) and if so just add this resource path to that one.
		// Otherwise add another backfill to the list.
		var compatibleBackfill *backfillDescription
		for _, backfill := range backfills {
			if backfill.CollectionID == collectionID && backfill.ResumeState.Equal(resourceState.Backfill) && slices.Equal(backfill.RestartCursorPath, resourceState.restartCursorPath) && backfill.RestartCursorValue == resourceState.RestartCursorValue {
				compatibleBackfill = backfill
				break
			}
		}
		if compatibleBackfill != nil {
			compatibleBackfill.ResourcePaths = append(compatibleBackfill.ResourcePaths, resourceState.path)
		} else {
			backfills = append(backfills, &backfillDescription{
				CollectionID:       collectionID,
				ResourcePaths:      []string{resourceState.path},
				ResumeState:        resourceState.Backfill,
				RestartCursorPath:  resourceState.restartCursorPath,
				RestartCursorValue: resourceState.RestartCursorValue,
			})
		}
	}

	// Connect to Firestore gRPC API
	var credsOpt = option.WithCredentialsJSON([]byte(c.Config.CredentialsJSON))
	var scopesOpt = option.WithScopes(firebaseScopes...)
	rpcClient, err := firestore_v1.NewClient(ctx, credsOpt, scopesOpt)
	if err != nil {
		return err
	}
	defer rpcClient.Close()

	// If we're going to perform any async backfills, connect to Firestore via the client library too
	var libraryClient *firestore.Client
	if len(backfills) > 0 {
		log.WithField("backfills", len(backfills)).Debug("opening second firestore client for async backfills")
		app, err := firebase.NewApp(ctx, nil, credsOpt)
		if err != nil {
			return err
		}
		libraryClient, err = app.Firestore(ctx)
		if err != nil {
			return err
		}
		defer libraryClient.Close()
	}

	// If the 'database' config property is unspecified, try to autodetect it from
	// the provided credentials.
	if c.Config.DatabasePath == "" {
		var creds, _ = transport.Creds(ctx, credsOpt)
		if creds == nil || creds.ProjectID == "" {
			return fmt.Errorf("unable to determine project ID (set 'database' config property)")
		}
		c.Config.DatabasePath = fmt.Sprintf("projects/%s/databases/(default)", creds.ProjectID)
		log.WithField("path", c.Config.DatabasePath).Warn("using autodetected database path (set 'database' config property to override)")
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "google-cloud-resource-prefix", c.Config.DatabasePath)

	// Notify Flow that we're starting.
	if err := c.Output.Ready(false); err != nil {
		return err
	}

	// Emit the initial state checkpoint, as this may differ from the previous
	// state when bindings are removed.
	if checkpointJSON, err := json.Marshal(c.State); err != nil {
		return fmt.Errorf("error serializing state checkpoint: %w", err)
	} else if err := c.Output.Checkpoint(checkpointJSON, false); err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"bindings":       len(c.State.Resources),
		"watches":        len(watchCollections),
		"asyncBackfills": len(backfills),
	}).Info("capture starting")
	for collectionID, startTime := range watchCollections {
		var collectionID, startTime = collectionID, startTime // Copy the loop variables for each closure
		log.WithField("collection", collectionID).Debug("starting worker")
		eg.Go(func() error {
			if err := c.StreamChanges(ctx, rpcClient, collectionID, startTime); err != nil {
				return fmt.Errorf("error streaming changes for collection %q: %w", collectionID, err)
			}
			return nil
		})
	}
	for _, backfill := range backfills {
		var backfill = backfill // Copy loop variable for each closure
		log.WithFields(log.Fields{
			"collection": backfill.CollectionID,
			"resources":  backfill.ResourcePaths,
		}).Debug("starting backfill worker")
		eg.Go(func() error {
			if err := c.BackfillAsync(ctx, libraryClient, backfill); err != nil {
				return fmt.Errorf("error backfilling collection %q: %w", backfill.CollectionID, err)
			}
			return nil
		})
	}
	defer log.Info("capture terminating")
	if err := eg.Wait(); err != nil && !errors.Is(err, io.EOF) {
		log.WithField("err", err).Error("capture worker failed")
		return err
	}
	return nil
}

type backfillDescription struct {
	CollectionID  collectionGroupID // The collection group ID this backfill will query
	ResourcePaths []resourcePath    // All resource paths which will be captured by this backfill
	ResumeState   *backfillState    // The backfill state from which to resume

	RestartCursorPath  []string // The parsed path to the restart cursor property, or nil if there is no such property.
	RestartCursorValue any      // The most recent restart cursor value from this resource, or nil if there is no such value.
}

func (c *capture) BackfillAsync(ctx context.Context, client *firestore.Client, backfill *backfillDescription) error {
	var logEntry = log.WithFields(log.Fields{
		"collection": backfill.CollectionID,
		"resources":  backfill.ResourcePaths,
	})

	// This should never happen since we only run BackfillAsync when there's a
	// backfill to perform, but seemed safe enough to check anyway.
	var resumeState = backfill.ResumeState
	if resumeState == nil || resumeState.Completed {
		logEntry.Warn("internal error: no backfill necessary")
		return nil
	}

	// If the StartAfter time is in the future then we need to wait until it's
	// the appropriate time.
	if dt := time.Until(resumeState.StartAfter); dt > 0 {
		logEntry.WithField("wait", dt.String()).Info("waiting to start backfill")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dt):
			// proceed to the rest of the function
		}
	}

	var cursor *firestore.DocumentSnapshot
	if resumeState.Cursor == "" {
		// If the cursor path is empty then we just leave the cursor document pointer nil
		logEntry.Info("starting async backfill")
	} else if resumeDocument, err := client.Doc(resumeState.Cursor).Get(ctx); err != nil {
		// If we fail to fetch the resume document, we clear the relevant backfill cursor and error out.
		// This will cause the backfill to restart from the beginning after the capture gets restarted,
		// and in the meantime it will show up as an error in the UI in case there's a persistent issue.
		resumeState.Cursor = ""
		if checkpointJSON, err := c.State.UpdateBackfillState(backfill.ResourcePaths, resumeState); err != nil {
			return err
		} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
			return err
		}
		return fmt.Errorf("restarting backfill %q: error fetching resume document %q", backfill.CollectionID, resumeState.Cursor)
	} else if !resumeDocument.UpdateTime.Equal(resumeState.MTime) {
		// Just like if the resume document fetch fails, mtime mismatches cause us to error out, so
		// we'll restart from the beginning when the connector gets restarted and in the meantime
		// it'll show up red in the UI.
		resumeState.Cursor = ""
		if checkpointJSON, err := c.State.UpdateBackfillState(backfill.ResourcePaths, resumeState); err != nil {
			return err
		} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
			return err
		}
		return fmt.Errorf("restarting backfill %q: resume document %q modified during backfill", backfill.CollectionID, resumeState.Cursor)
	} else {
		cursor = resumeDocument
	}

	// In order to limit the number of concurrent backfills we're buffering
	// in memory at any moment we use a semaphore. Instead of waiting to
	// acquire the semaphore before each query, we instead acquire it up-
	// front so that we can ensure that any return path from this function
	// will correctly release it. Then before each query we *release and
	// reacquire* the semaphore to give other backfills a chance to make
	// progress.
	if err := c.backfillSemaphore.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.backfillSemaphore.Release(1)

	var numDocuments int
	for {
		// Give other backfills a chance to acquire the semaphore, then take
		// it back for ourselves.
		c.backfillSemaphore.Release(1)
		if err := c.backfillSemaphore.Acquire(ctx, 1); err != nil {
			return err
		}

		// Block any further backfill work so long as any StreamChanges workers are
		// not fully caught up. Async backfills are not time-critical -- while it's
		// nice for them to finish as quickly as they can, nothing major will break
		// if a backfill takes a bit longer. Change streaming however *must* always
		// remain fully caught up or Very Bad Things happen.
		c.streamsInCatchup.Wait()

		var query firestore.Query = client.CollectionGroup(backfill.CollectionID).Query
		if backfill.RestartCursorPath != nil {
			query = query.OrderByPath(firestore.FieldPath(backfill.RestartCursorPath), firestore.Asc)
		}
		if cursor != nil {
			query = query.StartAfter(cursor)
		} else if backfill.RestartCursorPath != nil && backfill.RestartCursorValue != nil {
			var startAfter = backfill.RestartCursorValue
			if restartString, ok := backfill.RestartCursorValue.(string); ok {
				// If the RestartCursorValue is a string holding a valid RFC3339 timestamp then
				// assume that it's actually a typed timestamp in the source dataset.
				if ts, err := time.Parse(time.RFC3339Nano, restartString); err == nil {
					startAfter = ts
				}
			}
			query = query.StartAfter(startAfter)
		}
		query = query.Limit(backfillChunkSize)

		var docs, err = query.Documents(ctx).GetAll()
		if err != nil {
			if status.Code(err) == codes.Canceled {
				err = context.Canceled // Undo an awful bit of wrapping which breaks errors.Is()
			}
			return fmt.Errorf("error backfilling %q: chunk query failed after %d documents: %w", backfill.CollectionID, numDocuments, err)
		}
		logEntry.WithFields(log.Fields{
			"total": numDocuments,
			"chunk": len(docs),
		}).Debug("processing backfill documents")
		if len(docs) == 0 {
			break
		}

		for _, doc := range docs {
			logEntry.WithField("doc", doc.Ref.Path).Trace("got document")

			// We update the cursor before checking whether this document is being
			// backfilled. This does, unfortunately, mean that it's possible for changes
			// to a document which *isn't being captured* could break the ongoing backfill
			// resume behavior, but that's just how Firestore collection group queries
			// work.
			cursor = doc

			// The 'CollectionGroup' query is potentially over-broad, so skip documents
			// which aren't actually part of the current backfill.
			var resourcePath = documentToResourcePath(doc.Ref.Path)
			if !slices.Contains(backfill.ResourcePaths, resourcePath) {
				continue
			}

			// Convert the document into JSON-serializable form
			var fields = doc.Data()
			for key, value := range fields {
				fields[key] = sanitizeValue(value)
			}
			fields[metaProperty] = &documentMetadata{
				Path:       doc.Ref.Path,
				CreateTime: &doc.CreateTime,
				UpdateTime: &doc.UpdateTime,
				Snapshot:   true,
			}

			if bindingIndex, ok := c.State.BindingIndex(resourcePath); !ok {
				return fmt.Errorf("internal error: no binding index for async backfill of resource %q", resourcePath)
			} else if docJSON, err := json.Marshal(fields); err != nil {
				return fmt.Errorf("error serializing document %q: %w", doc.Ref.Path, err)
			} else if err := c.Output.Documents(bindingIndex, docJSON); err != nil {
				return err
			}
			numDocuments++
		}

		resumeState.Cursor = trimDatabasePath(cursor.Ref.Path)
		resumeState.MTime = cursor.UpdateTime
		logEntry.WithFields(log.Fields{
			"total":  numDocuments,
			"cursor": resumeState.Cursor,
			"mtime":  resumeState.MTime,
		}).Debug("updating backfill cursor")
		if checkpointJSON, err := c.State.UpdateBackfillState(backfill.ResourcePaths, resumeState); err != nil {
			return err
		} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
			return err
		}
	}

	logEntry.WithField("docs", numDocuments).Info("backfill complete")
	resumeState.Completed = true
	resumeState.Cursor = ""
	resumeState.MTime = time.Time{}
	if checkpointJSON, err := c.State.UpdateBackfillState(backfill.ResourcePaths, resumeState); err != nil {
		return err
	} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
		return err
	}
	return nil
}

func (c *capture) StreamChanges(ctx context.Context, client *firestore_v1.Client, collectionID string, readTime time.Time) error {
	var logEntry = log.WithFields(log.Fields{
		"collection": collectionID,
	})
	logEntry.WithField("readTime", readTime.Format(time.RFC3339)).Info("streaming changes from collection")

	var target = &firestore_pb.Target{
		TargetType: &firestore_pb.Target_Query{
			Query: &firestore_pb.Target_QueryTarget{
				Parent: c.Config.DatabasePath + `/documents`,
				QueryType: &firestore_pb.Target_QueryTarget_StructuredQuery{
					StructuredQuery: &firestore_pb.StructuredQuery{
						From: []*firestore_pb.StructuredQuery_CollectionSelector{{
							CollectionId:   collectionID,
							AllDescendants: true,
						}},
					},
				},
			},
		},
		TargetId: watchTargetID,
	}
	if !readTime.IsZero() {
		target.ResumeType = &firestore_pb.Target_ReadTime{
			ReadTime: timestamppb.New(readTime),
		}
	}
	var req = &firestore_pb.ListenRequest{
		Database: c.Config.DatabasePath,
		TargetChange: &firestore_pb.ListenRequest_AddTarget{
			AddTarget: target,
		},
	}

	var listenClient firestore_pb.Firestore_ListenClient
	var numRestarts, numDocuments int
	var isCurrent, catchupStreaming bool
	for {
		if listenClient == nil {
			var err error
			listenClient, err = client.Listen(ctx)
			if err != nil {
				return fmt.Errorf("error opening Listen RPC client: %w", err)
			} else if err := listenClient.Send(req); err != nil {
				return fmt.Errorf("error sending Listen RPC: %w", err)
			}

			logEntry.WithFields(log.Fields{
				"restarts":             numRestarts,
				"docsSinceLastRestart": numDocuments,
			}).Debug("opened listen stream")

			numRestarts++
			numDocuments = 0
			isCurrent = false
			if !catchupStreaming {
				catchupStreaming = true
				c.streamsInCatchup.Add(1)

				// Ensure that we call Done if there's an early return
				defer func() {
					if catchupStreaming {
						c.streamsInCatchup.Done()
					}
				}()
			}
		}

		resp, err := listenClient.Recv()
		if err == io.EOF {
			logEntry.Debug("listen stream closed, shutting down")
			return fmt.Errorf("listen stream was closed unexpectedly: %w", err)
		} else if status.Code(err) == codes.Canceled {
			logEntry.Debug("context canceled, shutting down")
			return context.Canceled // Undo an awful bit of wrapping which breaks errors.Is()
		} else if retryableStatus(err) {
			logEntry.WithFields(log.Fields{
				"err":  err,
				"docs": numDocuments,
			}).Errorf("retryable failure, will retry in %s", retryInterval)
			if err := listenClient.CloseSend(); err != nil {
				logEntry.WithField("err", err).Warn("error closing listen client")
			}
			time.Sleep(retryInterval)
			listenClient = nil
			continue
		} else if err != nil {
			return fmt.Errorf("error streaming %q changes: %w", collectionID, err)
		}
		logEntry.WithField("resp", resp).Trace("got response")

		var prevResumeToken string

		switch resp := resp.ResponseType.(type) {
		case *firestore_pb.ListenResponse_TargetChange:
			// This is an experimental bit of debugging-in-production logging added in April 2024
			// in order to investigate whether it might be possible to make incremental streaming
			// progress using resume tokens instead of read times when retrying.
			//
			// The official Firestore client library (which we don't use because it doesn't allow
			// us to stream incremental results before an entire consistent snapshot has been read
			// into memory) only updates the resume token when a consistent point is reached. But
			// it is theoretically permitted for the server to send useful resume tokens before we
			// reach that point, and if it does then this might allow us to more reliably stream
			// changes from high-throughput collections.
			if len(resp.TargetChange.ResumeToken) != 0 {
				var nextResumeToken = string(resp.TargetChange.ResumeToken)
				if nextResumeToken != prevResumeToken {
					logEntry.WithFields(log.Fields{
						"token":   nextResumeToken,
						"targets": resp.TargetChange.TargetIds,
					}).Debug("got new resume token")
					prevResumeToken = nextResumeToken
				}
			}

			logEntry.WithField("tc", resp.TargetChange).Trace("TargetChange Event")
			switch tc := resp.TargetChange; tc.TargetChangeType {
			case firestore_pb.TargetChange_NO_CHANGE:
				var ts = tc.ReadTime.AsTime().Format(time.RFC3339Nano)
				if log.IsLevelEnabled(log.TraceLevel) {
					logEntry.WithField("readTime", ts).Trace("TargetChange.NO_CHANGE")
				}
				if len(tc.TargetIds) == 0 && tc.ReadTime != nil && isCurrent {
					logEntry.WithFields(log.Fields{
						"readTime": ts,
						"docs":     numDocuments,
					}).Debug("consistent point reached")
					if catchupStreaming {
						logEntry.WithFields(log.Fields{
							"readTime": ts,
							"docs":     numDocuments,
						}).Info("stream caught up")
						catchupStreaming = false
						c.streamsInCatchup.Done()
					}
					target.ResumeType = &firestore_pb.Target_ReadTime{ReadTime: tc.ReadTime}
					if checkpointJSON, err := c.State.UpdateReadTimes(collectionID, tc.ReadTime.AsTime()); err != nil {
						return err
					} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
						return err
					}
				}
			case firestore_pb.TargetChange_ADD:
				logEntry.WithField("targets", tc.TargetIds).Trace("TargetChange.ADD")
				if len(tc.TargetIds) != 1 || tc.TargetIds[0] != watchTargetID {
					return fmt.Errorf("unexpected target ID %d", tc.TargetIds[0])
				}
			case firestore_pb.TargetChange_REMOVE:
				listenClient = nil
				if catchupStreaming {
					logEntry.WithField("docs", numDocuments).Warn("replication failed to catch up, skipping to latest changes (go.estuary.dev/YRDsKd)")
					var checkpointJSON, allStreamsHaveRestartCursors, err = c.State.MarkInconsistent(collectionID)
					if err != nil {
						return err
					} else if err := c.Output.Checkpoint(checkpointJSON, true); err != nil {
						return err
					}
					// The logic to re-backfill an inconsistent stream only triggers at connector startup,
					// so we need to eventually trigger a restart to ensure that it takes effect. This is
					// a heuristic which tries to ensure that we restart at the right time, but doesn't
					// need to be perfect. Note that the connector could restart at any time before this
					// timer fires anyway.
					var backfillRestartDelay = backfillRestartDelayNoRestartCursor
					if allStreamsHaveRestartCursors {
						backfillRestartDelay = backfillRestartDelayWithRestartCursor
					}
					time.AfterFunc(backfillRestartDelay+time.Hour, func() {
						logEntry.Fatal("forcing connector restart to establish consistency")
					})
					target.ResumeType = &firestore_pb.Target_ReadTime{ReadTime: timestamppb.New(time.Now())}
				} else if tc.Cause != nil {
					logEntry.WithField("cause", tc.Cause.Message).Warn("unexpected TargetChange.REMOVE")
					time.Sleep(retryInterval)
				} else {
					logEntry.Warn("unexpected TargetChange.REMOVE")
					time.Sleep(retryInterval)
				}
			case firestore_pb.TargetChange_CURRENT:
				if log.IsLevelEnabled(log.TraceLevel) {
					var ts = resp.TargetChange.ReadTime.AsTime().Format(time.RFC3339Nano)
					logEntry.WithField("readTime", ts).Trace("TargetChange.CURRENT")
				}
				isCurrent = true
			default:
				return fmt.Errorf("unhandled TargetChange (%s)", tc)
			}
		case *firestore_pb.ListenResponse_DocumentChange:
			if len(resp.DocumentChange.RemovedTargetIds) != 0 {
				return fmt.Errorf("internal error: removed target IDs %v", resp.DocumentChange.RemovedTargetIds)
			}
			var doc = resp.DocumentChange.Document
			var resourcePath = documentToResourcePath(doc.Name)
			if getLastCollectionGroupID(resourcePath) != collectionID {
				// This should never happen, but is an opportunistic sanity check to ensure
				// that we're receiving documents on the goroutines which requested them. If
				// this fails it likely means that Firestore has changed some details of how
				// the gRPC 'Listen' API works.
				return fmt.Errorf("internal error: recieved document %q on listener for %q", doc.Name, collectionID)
			}
			numDocuments++
			if numDocuments%progressLogInterval == 0 {
				logEntry.WithField("docs", numDocuments).Debug("replication progress")
			}
			if err := c.HandleDocument(ctx, resourcePath, doc); err != nil {
				return err
			}
		case *firestore_pb.ListenResponse_DocumentDelete:
			var doc = resp.DocumentDelete.Document
			var readTime = resp.DocumentDelete.ReadTime.AsTime()
			var resourcePath = documentToResourcePath(doc)
			numDocuments++
			if numDocuments%progressLogInterval == 0 {
				logEntry.WithField("docs", numDocuments).Debug("replication progress")
			}
			if err := c.HandleDelete(ctx, resourcePath, doc, readTime); err != nil {
				return err
			}
		case *firestore_pb.ListenResponse_DocumentRemove:
			var doc = resp.DocumentRemove.Document
			var readTime = resp.DocumentRemove.ReadTime.AsTime()
			var resourcePath = documentToResourcePath(doc)
			numDocuments++
			if numDocuments%progressLogInterval == 0 {
				logEntry.WithField("docs", numDocuments).Debug("replication progress")
			}
			if err := c.HandleDelete(ctx, resourcePath, doc, readTime); err != nil {
				return err
			}
		case *firestore_pb.ListenResponse_Filter:
			logEntry.WithField("filter", resp.Filter).Debug("ListenResponse.Filter")
		default:
			return fmt.Errorf("unhandled ListenResponse: %v", resp)
		}
	}
}

// When any watch stream reaches a consistent point a checkpoint is emitted to
// update the 'Read Time' associated with the impacted bindings. However, the
// first consistent point only occurs after the initial state of the dataset is
// fully synced, and this could potentially be many gigabytes of data.
//
// By emitting empty checkpoints periodically during the capture we unblock Flow
// to persist our capture output instead of buffering everything, at the cost of
// potentially duplicating documents in the event of a connector restart. I think
// this is the best we can do, given the Firestore APIs and the constraint of not
// buffering the entire dataset locally.
const emptyCheckpoint string = `{}`

func (c *capture) HandleDocument(ctx context.Context, resourcePath string, doc *firestore_pb.Document) error {
	// Ignore document changes which occurred prior to the last read time of the collection.
	var ctime = doc.CreateTime.AsTime()            // The time at which this document was first created
	var mtime = doc.UpdateTime.AsTime()            // The time at which this document was last modified
	var rtime, ok = c.State.ReadTime(resourcePath) // The latest read time for this resource path
	if lvl := log.TraceLevel; log.IsLevelEnabled(lvl) {
		log.WithFields(log.Fields{
			"doc":   doc.Name,
			"ctime": ctime.Format(time.RFC3339Nano),
			"mtime": mtime.Format(time.RFC3339Nano),
			"rtime": rtime.Format(time.RFC3339Nano),
			"res":   resourcePath,
		}).Log(lvl, "document change")
	}
	if !ok {
		log.WithField("doc", doc.Name).Trace("ignoring document (resource not captured)")
		return nil
	}
	if delta := mtime.Sub(rtime); delta < 0 {
		log.WithField("doc", doc.Name).Trace("ignoring document (mtime < rtime)")
		return nil
	}

	// Convert the document into a JSON-serializable map of fields
	var fields = make(map[string]interface{})
	for id, val := range doc.Fields {
		var tval, err = translateValue(val)
		if err != nil {
			return fmt.Errorf("error translating value: %w", err)
		}
		fields[id] = tval
	}
	fields[metaProperty] = &documentMetadata{
		Path:       doc.Name,
		CreateTime: &ctime,
		UpdateTime: &mtime,
	}

	if bindingIndex, ok := c.State.BindingIndex(resourcePath); !ok {
		// Listen streams can be a bit over-broad. For instance if there are
		// collections 'users/*/docs' and 'groups/*/docs' in the database, but
		// only 'users/*/docs' is captured, we need to ignore any documents
		// from paths like 'groups/*/docs' which don't map to any binding.
		return nil
	} else if docJSON, err := json.Marshal(fields); err != nil {
		return fmt.Errorf("error serializing document %q: %w", doc.Name, err)
	} else if err := c.Output.Documents(bindingIndex, docJSON); err != nil {
		return err
	}

	var checkpointUpdate = json.RawMessage(emptyCheckpoint)

	// Update restart-cursor state by promoting a pending value to current if necessary
	// and by buffering a new value if a cursor path is specified.
	//
	// TODO(wgd): Consider factoring this out as a helper function like other state accesses?
	c.State.Lock()
	if sk, ok := c.State.stateKeys[resourcePath]; ok {
		if state := c.State.Resources[sk]; state != nil {
			if !state.pendingRestartCursorTimeout.IsZero() && time.Now().Before(state.pendingRestartCursorTimeout) {
				state.RestartCursorValue = state.pendingRestartCursorValue
				state.pendingRestartCursorTimeout = time.Time{}
				state.pendingRestartCursorValue = nil

				var updated = map[boilerplate.StateKey]*resourceState{sk: state}
				var updateJSON, err = json.Marshal(&captureState{Resources: updated})
				if err != nil {
					return fmt.Errorf("error serializing state checkpoint: %w", err)
				}
				checkpointUpdate = updateJSON
			}
			if len(state.restartCursorPath) > 0 && state.pendingRestartCursorTimeout.IsZero() {
				if restartCursorValue, ok := indexDocumentByPath(fields, state.restartCursorPath); ok {
					state.pendingRestartCursorTimeout = time.Now().Add(restartCursorPendingInterval)
					state.pendingRestartCursorValue = restartCursorValue
				}
			}
		}
	}
	c.State.Unlock()

	if err := c.Output.Checkpoint(checkpointUpdate, true); err != nil {
		return err
	}
	return nil
}

// indexDocumentByPath implements JSON pointer indexing rules given a parsed
// list of individual path elements.
func indexDocumentByPath(x any, path []string) (any, bool) {
	for _, p := range path {
		if obj, ok := x.(map[string]any); ok {
			var fval, ok = obj[p]
			if !ok {
				return nil, false
			}
			x = fval
		} else if arr, ok := x.([]any); ok {
			if idx, err := strconv.Atoi(p); err != nil {
				return nil, false
			} else if idx < 0 || idx >= len(arr) {
				return nil, false
			} else {
				x = arr[idx]
			}
		} else {
			return nil, false
		}
	}
	return x, true
}

func (c *capture) HandleDelete(ctx context.Context, resourcePath string, docName string, readTime time.Time) error {
	if lvl := log.TraceLevel; log.IsLevelEnabled(lvl) {
		log.WithFields(log.Fields{
			"doc":   docName,
			"mtime": readTime.Format(time.RFC3339Nano),
			"res":   resourcePath,
		}).Log(lvl, "document delete")
	}

	var bindingIndex, ok = c.State.BindingIndex(resourcePath)
	if !ok {
		return nil
	}
	var fields = map[string]interface{}{
		metaProperty: &documentMetadata{
			Path:       docName,
			UpdateTime: &readTime,
			Deleted:    true,
		},
	}
	if docJSON, err := json.Marshal(fields); err != nil {
		return fmt.Errorf("error serializing deletion record %q: %w", docName, err)
	} else if err := c.Output.Documents(bindingIndex, docJSON); err != nil {
		return err
	} else if err := c.Output.Checkpoint(json.RawMessage(emptyCheckpoint), true); err != nil {
		return err
	}
	return nil
}

func translateValue(val *firestore_pb.Value) (interface{}, error) {
	switch val := val.ValueType.(type) {
	case *firestore_pb.Value_NullValue:
		return nil, nil
	case *firestore_pb.Value_BooleanValue:
		return val.BooleanValue, nil
	case *firestore_pb.Value_IntegerValue:
		return val.IntegerValue, nil
	case *firestore_pb.Value_DoubleValue:
		if math.IsNaN(val.DoubleValue) {
			return "NaN", nil
		} else if math.IsInf(val.DoubleValue, +1) {
			return "Infinity", nil
		} else if math.IsInf(val.DoubleValue, -1) {
			return "-Infinity", nil
		}
		return val.DoubleValue, nil
	case *firestore_pb.Value_TimestampValue:
		return val.TimestampValue.AsTime(), nil
	case *firestore_pb.Value_StringValue:
		return val.StringValue, nil
	case *firestore_pb.Value_BytesValue:
		return val.BytesValue, nil
	case *firestore_pb.Value_ReferenceValue:
		// TODO(wgd): Is it okay/good to flatten the string-vs-reference distinction here?
		// My gut says yes, in general we probably want to just coerce document references
		// into the name/path of that document as a string, but I can see an argument for
		// turning references into some sort of object instead.
		return val.ReferenceValue, nil
	case *firestore_pb.Value_GeoPointValue:
		return val.GeoPointValue, nil
	case *firestore_pb.Value_ArrayValue:
		var xs = make([]interface{}, len(val.ArrayValue.Values))
		for i, v := range val.ArrayValue.Values {
			var x, err = translateValue(v)
			if err != nil {
				return nil, err
			}
			xs[i] = x
		}
		return xs, nil
	case *firestore_pb.Value_MapValue:
		var xs = make(map[string]interface{}, len(val.MapValue.Fields))
		for k, v := range val.MapValue.Fields {
			var x, err = translateValue(v)
			if err != nil {
				return nil, err
			}
			xs[k] = x
		}
		return xs, nil
	}
	return nil, fmt.Errorf("unknown value type %T", val)
}

func sanitizeValue(x interface{}) interface{} {
	switch x := x.(type) {
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		} else if math.IsInf(x, +1) {
			return "Infinity"
		} else if math.IsInf(x, -1) {
			return "-Infinity"
		}
	case []interface{}:
		for idx, value := range x {
			x[idx] = sanitizeValue(value)
		}
		return x
	case map[string]interface{}:
		for key, value := range x {
			x[key] = sanitizeValue(value)
		}
		return x
	case *firestore.DocumentRef:
		return x.Path
	}
	return x
}
