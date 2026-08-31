package kvstore

import (
	"errors"
	"io"
	"os"
	"sort"
	"sync"
)

// simPageSize is the granularity at which the simulated disk makes bytes
// durable. Real devices commit in sectors, not in whatever size the caller
// happened to pass to write(2), and modelling that is what makes "only part
// of this write reached the platter" expressible at all.
const simPageSize = 512

// simDisk is a filesystem with two layers, and it is the reason clause B2 is
// checkable rather than merely stated.
//
// Every WriteAt lands in the PENDING layer, split on simPageSize boundaries.
// Sync promotes a file's pending pages into the DURABLE layer. Crash throws
// the pending layer away, which is what a power cut does. A read sees durable
// overlaid with pending, because a process can of course read back its own
// unsynced writes - the page cache shows them to it. The difference only
// appears after the power goes, which is exactly where it appears on real
// hardware.
//
// The store cannot tell the difference between this and os. That is the point:
// with the fsync in walpolicy.go commit() deleted, every test in this package
// used to stay green, including the whole 240-seed crash corpus, because the
// only evidence of the fsync was an event the store emitted and a counter the
// store incremented. Under this disk, deleting that one line means the record
// never leaves the pending layer, so the simulated power cut takes it and the
// reopened store has never heard of the key.
//
// A newly created file gets the same treatment one level up. Its contents can
// be made durable by fsyncing it, but the directory entry that names it lives
// in the parent's metadata and is no more durable than any unsynced write
// until syncDir is called. Until then a power cut leaves the contents on the
// platter with nothing pointing at them, which is indistinguishable from the
// file never having existed - and is precisely the failure syncdir_unix.go
// exists to prevent. Here that is the `linked` map, and Crash() deletes every
// file that is not linked.
//
// What it models:
//   - file contents, and which of them survive a power cut
//   - the directory entry of a newly created file, and whether it survives
//   - a write made durable in pieces, in an order the store did not choose
//   - a page that was only partly on the platter when the power went
//
// What it deliberately does NOT model, so that no test here over-claims:
// rename and remove take effect durably at once. Modelling a rename reverting
// would mean keeping the superseded file, and the crash-during-checkpoint
// window it would open is already covered by TestPartialCheckpointIsIgnored.
//
// And the standing caveat on all of it: this is a model of a filesystem where
// the application owns directory-entry durability, which is POSIX. It says
// what the store DOES - that it performs a directory sync at the point it has
// to - not what NTFS does with the no-op it gets on Windows. That half is a
// claim about the platform and is stated as one, in syncdir_windows.go.
type simDisk struct {
	mu      sync.Mutex
	durable map[string][]byte
	pending map[string][]simPage
	linked  map[string]bool
	faults  map[string]*simFault

	syncs    int
	dirSyncs int
	crashes  int
}

// simPage is one page-aligned chunk of an unsynced write, in the order the
// store issued it.
type simPage struct {
	off  int64
	data []byte
}

// simFault is what the disk does the NEXT time Sync is called on one file. It
// stages the two crash shapes a process kill provably cannot produce - the
// corpus was measured to contain zero of either, see -corpus-shapes.
//
// The zero value of simFault is not useful; build one with a helper below.
type simFault struct {
	// reverse promotes the pending pages in the opposite order to the one the
	// store issued them in. A device with a volatile write cache is free to
	// complete queued writes in whatever order suits it, right up until an
	// fsync tells it not to, and the store must not assume otherwise.
	reverse bool
	// stop promotes only this many pages. -1 promotes all of them.
	stop int
	// tear truncates the last page promoted to this many bytes. -1 keeps it
	// whole. This is a page that was half written when the power went.
	tear int
}

// tornSync makes the next Sync promote only the first n bytes of the file's
// first pending page, and lose everything after it.
func tornSync(n int) simFault { return simFault{stop: 1, tear: n} }

// lastPageOnlySync makes the next Sync promote the LAST pending page and
// nothing else, so a later part of a write is durable while the earlier part
// of the same write is not. What that leaves on the platter is a hole: bytes
// inside the file's extent that were never written, which read back as zero.
func lastPageOnlySync() simFault { return simFault{reverse: true, stop: 1, tear: -1} }

func newSimDisk() *simDisk {
	return &simDisk{
		durable: map[string][]byte{},
		pending: map[string][]simPage{},
		linked:  map[string]bool{},
		faults:  map[string]*simFault{},
	}
}

// FS returns the store-facing view of this disk. dir is only ever used to
// build the paths that appear in traces and error messages.
func (d *simDisk) FS(dir string) fileSystem { return simFS{disk: d, dir: dir} }

// Crash discards every byte that has been written and not yet promoted by a
// Sync, and every file whose directory entry was never made durable by a
// syncDir. This is the power cut.
func (d *simDisk) Crash() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = map[string][]simPage{}
	d.faults = map[string]*simFault{}
	for name, linked := range d.linked {
		if !linked {
			delete(d.durable, name)
			delete(d.linked, name)
		}
	}
	d.crashes++
}

// FaultNextSync arms a fault for the next Sync on name.
func (d *simDisk) FaultNextSync(name string, f simFault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.faults[name] = &f
}

// DurableBytes is what would be on the platter if the power went right now.
func (d *simDisk) DurableBytes(name string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.durable[name]...)
}

// Names lists every file the disk holds, sorted.
func (d *simDisk) Names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.durable))
	for n := range d.durable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Clone is an independent copy of everything on the platter. Two recoveries
// have to run against separate copies, because the first Open repairs the
// directory - truncating the torn tail - and comparing a crashed directory
// with a repaired one would prove nothing.
func (d *simDisk) Clone() *simDisk {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := newSimDisk()
	for k, v := range d.durable {
		out.durable[k] = append([]byte(nil), v...)
		out.linked[k] = d.linked[k]
	}
	return out
}

// Syncs is how many times a file was actually fsynced. Reported by the disk,
// not by the store, which is the whole difference between this and Stats().
func (d *simDisk) Syncs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.syncs
}

// view is durable overlaid with everything still pending. Caller holds d.mu.
func (d *simDisk) view(name string) []byte {
	out := append([]byte(nil), d.durable[name]...)
	for _, p := range d.pending[name] {
		out = applyPage(out, p.off, p.data)
	}
	return out
}

// applyPage writes data at off, zero-filling any gap. A real filesystem does
// exactly this: bytes never written inside a file's extent read back as zero,
// which is why an out-of-order flush leaves a hole rather than a short file.
func applyPage(dst []byte, off int64, data []byte) []byte {
	end := int(off) + len(data)
	if end > len(dst) {
		dst = append(dst, make([]byte, end-len(dst))...)
	}
	copy(dst[off:], data)
	return dst
}

func (d *simDisk) writeAt(name string, p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[name]; !ok {
		return 0, &os.PathError{Op: "writeat", Path: name, Err: os.ErrNotExist}
	}
	// Split on absolute page boundaries, so the pieces line up with the units
	// a device would actually commit.
	total := len(p)
	for len(p) > 0 {
		n := simPageSize - int(off%simPageSize)
		if n > len(p) {
			n = len(p)
		}
		d.pending[name] = append(d.pending[name], simPage{off: off, data: append([]byte(nil), p[:n]...)})
		off += int64(n)
		p = p[n:]
	}
	return total, nil
}

func (d *simDisk) readAt(name string, p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[name]; !ok {
		return 0, &os.PathError{Op: "readat", Path: name, Err: os.ErrNotExist}
	}
	content := d.view(name)
	if off >= int64(len(content)) {
		return 0, io.EOF
	}
	n := copy(p, content[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// sync promotes this file's pending pages, applying any armed fault.
//
// It always clears the pending queue, fault or no fault: the pages a fault
// drops are pages the power cut caught in flight, and they are never coming.
func (d *simDisk) sync(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[name]; !ok {
		return &os.PathError{Op: "fsync", Path: name, Err: os.ErrNotExist}
	}
	d.syncs++

	pages := d.pending[name]
	delete(d.pending, name)

	order := make([]int, len(pages))
	for i := range order {
		order[i] = i
	}
	stop := len(pages)
	tear := -1

	if f := d.faults[name]; f != nil {
		delete(d.faults, name)
		if f.reverse {
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		if f.stop >= 0 && f.stop < stop {
			stop = f.stop
		}
		tear = f.tear
	}
	if stop > len(order) {
		stop = len(order)
	}

	content := d.durable[name]
	for i := range stop {
		p := pages[order[i]]
		data := p.data
		if tear >= 0 && i == stop-1 && tear < len(data) {
			data = data[:tear]
		}
		content = applyPage(content, p.off, data)
	}
	d.durable[name] = content
	return nil
}

func (d *simDisk) truncate(name string, size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	content, ok := d.durable[name]
	if !ok {
		return &os.PathError{Op: "truncate", Path: name, Err: os.ErrNotExist}
	}
	// The store only ever truncates a freshly opened handle and fsyncs
	// immediately afterwards, so there is nothing pending to reconcile and the
	// simple model is exact here: cut the durable content, drop anything
	// pending beyond the new size.
	if int64(len(content)) > size {
		d.durable[name] = content[:size]
	} else {
		d.durable[name] = applyPage(content, size, nil)
	}
	kept := d.pending[name][:0]
	for _, p := range d.pending[name] {
		if p.off+int64(len(p.data)) <= size {
			kept = append(kept, p)
		}
	}
	d.pending[name] = kept
	return nil
}

// simFS is the fileSystem view of a simDisk.
type simFS struct {
	disk *simDisk
	dir  string
}

func (s simFS) path(name string) string { return s.dir + "/" + name }

func (s simFS) ensureDir() error { return nil }

func (s simFS) create(name string) (file, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.durable[name]; ok {
		return nil, &os.PathError{Op: "create", Path: name, Err: os.ErrExist}
	}
	s.disk.durable[name] = nil
	// A brand new directory entry, and not a durable one until syncDir.
	s.disk.linked[name] = false
	return simFile{disk: s.disk, name: name}, nil
}

func (s simFS) createTrunc(name string) (file, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.durable[name]; !ok {
		s.disk.linked[name] = false
	}
	s.disk.durable[name] = nil
	delete(s.disk.pending, name)
	return simFile{disk: s.disk, name: name}, nil
}

func (s simFS) open(name string) (file, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.durable[name]; !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return simFile{disk: s.disk, name: name}, nil
}

func (s simFS) size(name string) (int64, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.durable[name]; !ok {
		return 0, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	return int64(len(s.disk.view(name))), nil
}

func (s simFS) list() ([]string, error) { return s.disk.Names(), nil }

func (s simFS) remove(name string) error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.durable[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	delete(s.disk.durable, name)
	delete(s.disk.pending, name)
	delete(s.disk.linked, name)
	return nil
}

func (s simFS) rename(from, to string) error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	content, ok := s.disk.durable[from]
	if !ok {
		return &os.PathError{Op: "rename", Path: from, Err: os.ErrNotExist}
	}
	// Rename carries the pending layer with it, because on a real filesystem
	// it moves a directory entry and does not touch the file's data at all.
	s.disk.durable[to] = content
	s.disk.pending[to] = s.disk.pending[from]
	s.disk.linked[to] = true
	delete(s.disk.durable, from)
	delete(s.disk.pending, from)
	delete(s.disk.linked, from)
	return nil
}

// syncDir makes every directory entry durable. Until this is called, a file
// created since the last one does not survive Crash().
func (s simFS) syncDir() error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	s.disk.dirSyncs++
	for name := range s.disk.linked {
		s.disk.linked[name] = true
	}
	return nil
}

// simFile is one open handle. Handles on the same name share the disk's state,
// exactly as two descriptors on one file do.
type simFile struct {
	disk *simDisk
	name string
}

func (f simFile) WriteAt(p []byte, off int64) (int, error) {
	return f.disk.writeAt(f.name, p, off)
}

func (f simFile) ReadAt(p []byte, off int64) (int, error) { return f.disk.readAt(f.name, p, off) }
func (f simFile) Sync() error                             { return f.disk.sync(f.name) }
func (f simFile) Truncate(size int64) error               { return f.disk.truncate(f.name, size) }
func (f simFile) Close() error                            { return nil }

// findSimSegment returns the name of the live log segment, whatever it is
// based at, so a test that has checkpointed still finds it.
func findSimSegment(d *simDisk) (string, error) {
	var best string
	var bestBase uint64
	found := false
	for _, n := range d.Names() {
		base, ok := segmentBase(n)
		if !ok {
			continue
		}
		if !found || base >= bestBase {
			best, bestBase, found = n, base, true
		}
	}
	if !found {
		return "", errors.New("no log segment on the simulated disk")
	}
	return best, nil
}
