package kvstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrClosed is returned by any operation on a store that has been closed.
var ErrClosed = errors.New("kvstore: store is closed")

// defaultCheckpointBytes is how large a log segment is allowed to get before
// the store folds it into a checkpoint.
const defaultCheckpointBytes = 4 << 20

// Options configures a store.
type Options struct {
	// Dir is the directory the store owns. It is created if it does not exist.
	// Nothing else may write to it.
	Dir string

	// CheckpointBytes is how large the live log segment may get before the
	// store checkpoints. Zero selects the default.
	CheckpointBytes int64

	// trace is a test seam: it receives the store's internal events, in order,
	// as they happen. It is instrumentation on the real path rather than a
	// substitute for it - the fsync between sync-start and sync-return is a
	// real fsync on a real file. Unexported because the event names are not a
	// public contract.
	trace func(name, detail string)
}

// Stats are counters a caller can use to check the store is doing the work it
// claims to be doing - most usefully, that there really is one fsync per
// acknowledgement.
type Stats struct {
	Syncs    int64
	Records  int64
	LogBytes int64
}

// RecoveryReport says what the last Open found and, the part that matters,
// where and why it stopped reading.
type RecoveryReport struct {
	Segments       int
	Applied        int
	Skipped        int
	LastSeq        uint64
	Stopped        stopReason
	StoppedAt      string
	Dropped        int
	CheckpointSeq  uint64
	UsedCheckpoint bool
}

// Store is an embedded key-value store backed by a write-ahead log.
//
// It is safe for concurrent use, but there is no concurrency win to be had:
// every acknowledgement costs an fsync and they are serialised. One process at
// a time may hold a directory. See the README for what this deliberately is
// not.
type Store struct {
	mu     sync.RWMutex
	dir    string
	opts   Options
	data   map[string][]byte
	log    *wal
	report RecoveryReport
	stats  Stats
	closed bool
}

// Open opens or creates a store in dir with the default options.
func Open(dir string) (*Store, error) { return OpenWith(Options{Dir: dir}) }

// OpenWith opens or creates a store, replaying whatever log is already in the
// directory.
//
// A torn record at the end of the log is not an error: it is the expected
// shape of a log after a crash, and recovery stops cleanly at the last intact
// record. Check Recovery() to see whether that happened.
func OpenWith(opts Options) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("kvstore: Options.Dir is empty")
	}
	if opts.CheckpointBytes == 0 {
		opts.CheckpointBytes = defaultCheckpointBytes
	}
	if err := ensureDir(opts.Dir); err != nil {
		return nil, err
	}

	st, err := loadDir(opts.Dir)
	if err != nil {
		return nil, err
	}

	s := &Store{dir: opts.Dir, opts: opts, data: st.data, report: st.report}

	// Segments beyond the point recovery stopped are unreachable: the chain
	// that would let us verify them is broken, so nothing in them can ever be
	// replayed again. Leaving them would only confuse the next recovery.
	if len(st.drop) > 0 {
		for _, n := range st.drop {
			if err := os.Remove(filepath.Join(opts.Dir, segmentName(n))); err != nil {
				return nil, fmt.Errorf("kvstore: removing unreachable segment: %w", err)
			}
		}
		if err := syncDir(opts.Dir); err != nil {
			return nil, fmt.Errorf("kvstore: syncing after removing unreachable segments: %w", err)
		}
	}

	if st.segments == 0 {
		s.log, err = createSegment(opts.Dir, 1, 0, 0, opts.trace)
	} else {
		s.log, err = reopenSegment(opts.Dir, st.active, st.lastSeq, st.lastCRC, st.activeBytes, opts.trace)
	}
	if err != nil {
		return nil, err
	}
	s.stats.Records = int64(st.report.Applied)
	s.stats.LogBytes = s.log.bytes
	return s, nil
}

func ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kvstore: checking store directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("kvstore: creating store directory: %w", err)
	}
	// The directory we just created is itself a new entry in *its* parent, and
	// that entry is no more durable than any other until the parent is synced.
	// Same reasoning as for the log segments, one level up.
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("kvstore: syncing the parent of the store directory: %w", err)
	}
	return nil
}

func (s *Store) emit(name, detail string) {
	if s.opts.trace != nil {
		s.opts.trace(name, detail)
	}
}

// Put stores value under key. It returns once the write is durable: the record
// is in the log and the fsync covering it has returned.
func (s *Store) Put(key string, value []byte) error {
	// Copied once, here. The store keeps this slice in its map for the life of
	// the key, and a caller reusing its buffer for the next write would
	// otherwise silently change a value it had already stored.
	return s.write(record{kind: kindPut, key: key, value: append([]byte(nil), value...)})
}

// Delete removes key. Like Put, it returns once the removal is durable - a
// delete that is not logged is a value that comes back after a crash.
func (s *Store) Delete(key string) error {
	return s.write(record{kind: kindDelete, key: key})
}

// write is the single place an acknowledgement happens, and the order of the
// three steps below is the whole of clause B2.
//
// The log append comes first and it does not return until its fsync has
// returned. Only then is the in-memory map updated, and only then does this
// function return - which is the acknowledgement. Moving the map update one
// line earlier would make a reader see a value that a crash could still take
// away; moving the log append one line later would make the acknowledgement a
// promise about the page cache.
func (s *Store) write(r record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}

	batch := [1]record{r}
	if err := s.log.appendRecords(batch[:]); err != nil {
		return err
	}

	s.apply(r)
	s.stats.Syncs = s.log.syncs
	s.stats.Records++
	s.stats.LogBytes = s.log.bytes
	s.emit("ack", r.key)
	return nil
}

func (s *Store) apply(r record) {
	if r.kind == kindDelete {
		delete(s.data, r.key)
		return
	}
	s.data[r.key] = r.value
}

// Get returns a copy of the value stored under key.
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// Len returns the number of live keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// Stats returns the store's counters.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// Recovery returns the report from the Open that produced this store.
func (s *Store) Recovery() RecoveryReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.report
}

// Close releases the store's files. It is not required for durability -
// everything Put acknowledged is already on disk - so a process that dies
// without calling it loses nothing that was acknowledged.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.log.close()
}

// Snapshot returns a deterministic serialisation of the live key set: the same
// contents always produce the same bytes, whatever order they were written in
// and whatever the map's iteration order happens to be this run.
//
// It is what clause B4 is compared with, and it is the payload a checkpoint
// writes. See state.go for the format.
func (s *Store) Snapshot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return encodeState(s.data)
}
