package kvstore

// First cut: a map. Nothing here survives anything, which is the point - the
// tests in durability_test.go describe what has to become true, and every one
// of them fails against this. The write-ahead log replaces the body of Put and
// the body of OpenWith; the shape of the API does not change.

const ackAfterSync = true

// Options configures a store.
type Options struct {
	// Dir is the directory the store owns. It is created if missing.
	Dir string

	// CheckpointBytes is how large a log segment may get before the store
	// folds it into a checkpoint. Zero means the default.
	CheckpointBytes int64

	// trace is a test seam: it receives the store's internal events, in order,
	// as they happen. It is instrumentation on the real path rather than a
	// substitute for it - the fsync between sync-start and sync-return is a
	// real fsync. Unexported because it is not part of the public contract.
	trace func(name, detail string)
}

// Stats are counters a caller can use to check that the store is doing the
// work it claims to be doing.
type Stats struct {
	Syncs    int64
	Records  int64
	LogBytes int64
}

// RecoveryReport says what the last Open found, and - the part that matters -
// where and why it stopped reading.
type RecoveryReport struct {
	Segments       int
	Applied        int
	Skipped        int
	LastSeq        uint64
	Stopped        stopReason
	StoppedAt      string
	CheckpointSeq  uint64
	UsedCheckpoint bool
}

// Store is an embedded key-value store backed by a write-ahead log.
type Store struct {
	dir    string
	data   map[string][]byte
	report RecoveryReport
}

// Open opens or creates a store in dir with the default options.
func Open(dir string) (*Store, error) { return OpenWith(Options{Dir: dir}) }

// OpenWith opens or creates a store, recovering any log already in the
// directory.
func OpenWith(opts Options) (*Store, error) {
	return &Store{dir: opts.Dir, data: map[string][]byte{}}, nil
}

// Put stores value under key and returns once the write is durable.
func (s *Store) Put(key string, value []byte) error {
	s.data[key] = append([]byte(nil), value...)
	return nil
}

// Delete removes key and returns once the removal is durable.
func (s *Store) Delete(key string) error {
	delete(s.data, key)
	return nil
}

// Get returns the value stored under key.
func (s *Store) Get(key string) ([]byte, bool) {
	v, ok := s.data[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// Len returns the number of live keys.
func (s *Store) Len() int { return len(s.data) }

// Stats returns the store's counters.
func (s *Store) Stats() Stats { return Stats{} }

// Recovery returns the report from the last Open.
func (s *Store) Recovery() RecoveryReport { return s.report }

// Close releases the store's files.
func (s *Store) Close() error { return nil }
