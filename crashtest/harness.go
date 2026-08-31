// Package crashtest kills a real process at a randomised point and checks what
// the store looks like afterwards.
//
// The shape is: a parent forks a child, the child writes a schedule derived
// entirely from a seed and prints one line per acknowledged write, the parent
// kills it at an offset also derived from that seed, and then the parent
// reopens the directory and checks three things.
//
//	B1  every acknowledged write is there, and the one operation that was
//	    in flight when the process died may have happened or not
//	B3  no value ever comes back that the schedule did not write
//	B4  two independent first-replays of the crashed directory produce
//	    byte-identical state
//
// What this proves and what it does not, stated plainly because the difference
// is the whole point of the exercise:
//
// It proves the store survives its process dying at an arbitrary instruction -
// mid-write, mid-fsync, mid-rename, mid-delete. That is a real and useful
// property and it is where torn records, half-finished checkpoints and
// unreachable segments come from.
//
// It does NOT prove the fsync is doing its job. After Process.Kill the page
// cache is untouched and the kernel writes out unsynced data anyway, so a store
// that never called fsync at all would sail through this corpus. Only losing
// power catches that, and nothing in user space can arrange it. The fsync
// ordering is proven by the trace assertion in the main package's
// TestFsyncPrecedesAck, and beyond that by reading the code.
package crashtest

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chickengamer555/kvstore"
)

// Environment variables the parent sets on the child. A process that finds
// EnvSeed set is a child and must run the schedule instead of whatever it
// would normally do.
const (
	EnvSeed   = "KV_CRASH_SEED"
	EnvDir    = "KV_CRASH_DIR"
	EnvMaxOps = "KV_CRASH_MAXOPS"
)

const (
	// DefaultMaxOps caps the schedule so a child that is never killed still
	// terminates. Every seed in the corpus is killed long before this.
	DefaultMaxOps = 600

	// CheckpointBytes is small on purpose. Checkpointing is the one path that
	// deletes data deliberately, so it is where crash-safety bugs concentrate;
	// a bound this small means the corpus kills children inside a checkpoint
	// regularly rather than by luck.
	CheckpointBytes = 16 << 10

	childTimeout = 60 * time.Second
)

// Op is one operation in a seeded schedule.
type Op struct {
	Key    string
	Value  []byte
	Delete bool
}

// OpAt returns operation i of the schedule for seed.
//
// It is a pure function of (seed, i) and holds no state, which is what lets the
// parent work out what the child must have written without simulating it - and
// without trusting anything the child said beyond how far it got.
func OpAt(seed uint64, i int) Op {
	var in [16]byte
	binary.LittleEndian.PutUint64(in[0:], seed)
	binary.LittleEndian.PutUint64(in[8:], uint64(i))
	h := sha256.Sum256(in[:])

	key := fmt.Sprintf("k%04d", binary.LittleEndian.Uint16(h[0:])%600)
	// Deletes, but not in the first few operations - a schedule that opens by
	// deleting from an empty store tests very little.
	if i > 20 && h[2]%20 < 3 {
		return Op{Key: key, Delete: true}
	}
	n := 8 + int(h[3])%192
	val := make([]byte, n)
	for j := range val {
		val[j] = h[4+(j%28)] ^ byte(j)
	}
	return Op{Key: key, Value: val}
}

// RunChild is the child half. It writes the schedule for seed into dir,
// printing one acknowledgement line to ack after every operation the store has
// confirmed durable.
//
// The line goes straight to the file descriptor with no buffering in front of
// it. It is the parent's only record of what was acknowledged, and it has to
// survive the process being killed microseconds later; a bufio.Writer here
// would lose exactly the lines that matter most and would make the store look
// better than it is.
func RunChild(dir string, seed uint64, maxOps int, ack io.Writer) error {
	s, err := kvstore.OpenWith(kvstore.Options{Dir: dir, CheckpointBytes: CheckpointBytes})
	if err != nil {
		return fmt.Errorf("child: opening store: %w", err)
	}
	for i := range maxOps {
		op := OpAt(seed, i)
		if op.Delete {
			err = s.Delete(op.Key)
		} else {
			err = s.Put(op.Key, op.Value)
		}
		if err != nil {
			return fmt.Errorf("child: op %d: %w", i, err)
		}
		if _, err := fmt.Fprintf(ack, "ack %d\n", i); err != nil {
			return fmt.Errorf("child: reporting ack %d: %w", i, err)
		}
	}
	return s.Close()
}

// ChildFromEnv runs the child workload if this process was started as one.
//
// Both entry points call it first: the test binary re-executes itself
// (os.Args[0]) for the corpus, and cmd/crashrepro re-executes itself for the
// reproduce command and for the deliberately broken build.
func ChildFromEnv() (bool, error) {
	raw := os.Getenv(EnvSeed)
	if raw == "" {
		return false, nil
	}
	var seed uint64
	if _, err := fmt.Sscan(raw, &seed); err != nil {
		return true, fmt.Errorf("child: bad %s=%q: %w", EnvSeed, raw, err)
	}
	dir := os.Getenv(EnvDir)
	if dir == "" {
		return true, fmt.Errorf("child: %s is not set", EnvDir)
	}
	maxOps := DefaultMaxOps
	if raw := os.Getenv(EnvMaxOps); raw != "" {
		if _, err := fmt.Sscan(raw, &maxOps); err != nil {
			return true, fmt.Errorf("child: bad %s=%q: %w", EnvMaxOps, raw, err)
		}
	}
	return true, RunChild(dir, seed, maxOps, os.Stdout)
}

// Result is what one seed produced.
type Result struct {
	Seed uint64
	Dir  string

	// KillAfterAcks and KillJitter are the randomised offset, both derived
	// from the seed. The first is exact and reproducible; the second is a
	// delay, so where inside the following operation the signal actually lands
	// is not - see Reproducibility in the README.
	KillAfterAcks int
	KillJitter    time.Duration

	Acked        int
	FinishedFree bool // the child ran out of schedule before it could be killed
	Report       kvstore.RecoveryReport

	Failures []string
}

// OK reports whether this seed passed.
func (r Result) OK() bool { return len(r.Failures) == 0 }

// Kinds returns the sorted, deduplicated failure categories - what a
// reproduction has to match. The exact keys involved shift between runs of the
// same seed because the instant the signal lands is not under the harness's
// control; the category does not.
func (r Result) Kinds() []string {
	seen := map[string]bool{}
	for _, f := range r.Failures {
		seen[strings.SplitN(f, ":", 2)[0]] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r Result) String() string {
	status := "pass"
	if !r.OK() {
		status = "FAIL " + strings.Join(r.Kinds(), ",")
	}
	return fmt.Sprintf("seed=%d killAfterAcks=%d jitter=%s acked=%d stopped=%s %s",
		r.Seed, r.KillAfterAcks, r.KillJitter, r.Acked, r.Report.Stopped, status)
}

// Child is the process to fork. Argv[0] is the executable; the corpus uses the
// test binary itself, and the negative control uses a separately built one.
type Child struct {
	Argv []string
	Env  []string
}

// RunSeed forks the child, kills it at the offset this seed dictates, and
// checks the directory it left behind.
//
// Not implemented yet - crashtest/crash_test.go says what it has to do.
func RunSeed(child Child, seed uint64, dir string) (Result, error) {
	_, _, _ = child, seed, dir
	return Result{}, errors.New("crashtest: RunSeed is not implemented")
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
