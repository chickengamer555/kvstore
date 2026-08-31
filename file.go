package kvstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// file is every operation this store performs on a single open file.
//
// It exists for one reason, and it is the reason the rest of this package
// hangs on: durability has to be an observable property of the file rather
// than a claim the store makes about itself. Before this interface existed,
// the evidence that Put fsynced was an event the store emitted and a counter
// the store incremented - so deleting the fsync from the commit path left the
// entire test suite green, including the crash corpus. That is a test that
// asserts the narration rather than the fact.
//
// With the seam, the test can supply a file whose bytes only become readable
// after a Sync. A commit path that skips the Sync loses the data at the next
// simulated power cut, exactly as it would on real hardware, and the test that
// reopens the store fails. See simdisk_test.go.
//
// Positional WriteAt rather than Write is deliberate. The log already knows
// the offset it is appending at, so the implicit cursor buys nothing, and an
// interface with no hidden state is far easier to model honestly.
type file interface {
	WriteAt(p []byte, off int64) (int, error)
	ReadAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
	Close() error
}

// fileSystem is the directory a store owns, and the namespace operations it
// performs inside it.
//
// The store never touches os directly: everything goes through here, so that
// a test can hand it a whole simulated directory and then take the power away.
// Its methods are unexported, which means only this package can implement it -
// this is a seam for the tests that ship with the store, not a plug-in point
// for callers.
type fileSystem interface {
	// ensureDir creates the directory if it is absent, and makes its own
	// directory entry durable.
	ensureDir() error

	// create makes a new, empty file. It fails if the name already exists;
	// checkpointLocked relies on that to catch a segment it would otherwise
	// silently reopen.
	create(name string) (file, error)

	// createTrunc makes a new file, discarding any existing content. Only the
	// checkpoint's temporary file wants this.
	createTrunc(name string) (file, error)

	// open opens an existing file for reading and writing.
	open(name string) (file, error)

	// size reports the length of name.
	size(name string) (int64, error)

	// list names the files directly inside the directory. Order is
	// unspecified; callers that care sort.
	list() ([]string, error)

	remove(name string) error
	rename(from, to string) error

	// syncDir makes the directory's own entries durable. On a platform with no
	// such call this is the honest no-op described in syncdir_windows.go.
	syncDir() error

	// path is where name lives, for traces and error messages.
	path(name string) string
}

// osFS is the production filesystem: a thin wrapper over the os package that
// adds no behaviour of its own. Every method here is one os call plus the
// error wrapping that call already deserved.
type osFS struct{ dir string }

func (o osFS) path(name string) string { return filepath.Join(o.dir, name) }

func (o osFS) ensureDir() error {
	if _, err := os.Stat(o.dir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kvstore: checking store directory: %w", err)
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return fmt.Errorf("kvstore: creating store directory: %w", err)
	}
	// The directory just created is itself a new entry in *its* parent, and
	// that entry is no more durable than any other until the parent is synced.
	// Same reasoning as for the log segments, one level up.
	if err := syncDir(filepath.Dir(o.dir)); err != nil {
		return fmt.Errorf("kvstore: syncing the parent of the store directory: %w", err)
	}
	return nil
}

func (o osFS) create(name string) (file, error) {
	return os.OpenFile(o.path(name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
}

func (o osFS) createTrunc(name string) (file, error) {
	return os.OpenFile(o.path(name), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
}

func (o osFS) open(name string) (file, error) {
	return os.OpenFile(o.path(name), os.O_RDWR, 0o644)
}

func (o osFS) size(name string) (int64, error) {
	info, err := os.Stat(o.path(name))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (o osFS) list() ([]string, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

func (o osFS) remove(name string) error { return os.Remove(o.path(name)) }

func (o osFS) rename(from, to string) error { return os.Rename(o.path(from), o.path(to)) }

func (o osFS) syncDir() error { return syncDir(o.dir) }

// readAll reads name in full, through the same interface the store writes
// with.
//
// Recovery goes through here rather than through os.ReadFile so that what it
// sees is exactly what a reopening process would see. Under the simulated disk
// that means the durable layer only: bytes written but never synced are not
// there to be read, which is the whole point.
func readAll(fsys fileSystem, name string) ([]byte, error) {
	n, err := fsys.size(name)
	if err != nil {
		return nil, err
	}
	f, err := fsys.open(name)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	read := 0
	var readErr error
	for read < len(buf) {
		m, err := f.ReadAt(buf[read:], int64(read))
		read += m
		if err != nil {
			// A short read at the end is the file having been shorter than
			// Stat said, which a concurrent truncation can do. Everything else
			// is a real failure and is reported.
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	if cerr := f.Close(); cerr != nil && readErr == nil {
		readErr = cerr
	}
	if readErr != nil {
		return nil, readErr
	}
	return buf[:read], nil
}
