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
// # Metadata is staged too, and that is the second layer
//
// A file's contents are one thing; the directory entry that names it is
// another, and the two are made durable by different calls. So this disk keeps
// the directory itself in two layers: dirLive is what a running process sees,
// dirDurable is what is on the platter. create, rename and remove change
// dirLive only. syncDir - fsync(2) on the containing directory - copies live
// over durable. Crash copies durable back over live, so an unsynced create
// vanishes, an unsynced rename reverts to the old name, and an unsynced remove
// brings the file back.
//
// Each of those is a state a real POSIX filesystem is permitted to leave, and
// each is a reason the application has to ask:
//
//   - create without syncDir: the file's contents are on the platter with
//     nothing pointing at them, which is indistinguishable from the file never
//     having existed. This is what syncdir_unix.go exists to prevent.
//   - rename without syncDir: rename(2) is atomic with respect to readers,
//     which is a different property from being durable. Until the directory is
//     synced, the old name is what a reopening process finds.
//   - remove without syncDir: unlink(2) is a directory write like any other,
//     and until the directory is synced the entry can come back.
//   - truncate without fsync: a file's length is inode metadata. ftruncate(2)
//     changes what this process reads; fsync on that descriptor is what makes
//     the new length survive.
//
// The model sits at the pessimistic end of what POSIX permits: an unsynced
// metadata change ALWAYS reverts here, where a real filesystem may or may not
// keep it. That direction is the safe one - a store that passes here passes
// under any weaker model - and it is the same stance Crash() already takes on
// unsynced data. It is not a source of failures a real disk cannot produce,
// because every state it stages is one a real disk is allowed to leave; it
// stages them every time rather than sometimes.
//
// What it models:
//   - file contents, and which of them survive a power cut
//   - the directory: creates, renames and removes, and whether each survives
//   - a file's length, and whether a truncation survives
//   - a write made durable in pieces, in an order the store did not choose
//   - a page that was only partly on the platter when the power went
//   - a filesystem call that fails, and a write that is short
//
// What it deliberately does NOT model, so that no test here over-claims:
//
//   - Partial metadata. A directory sync here promotes every pending entry at
//     once. A real filesystem journals in transactions and may commit some of
//     a directory's changes without the rest.
//   - Two truncations of one file with no fsync between them: the second
//     replaces the first. The store never does this.
//   - Ordering between two different files' data. Each file's pending layer is
//     promoted only by its own fsync, which is right, but nothing here models
//     a device reordering across files.
//
// And the standing caveat on all of it: this is a model of a filesystem where
// the application owns directory-entry durability, which is POSIX. It says
// what the store DOES - that it performs a directory sync at the point it has
// to - not what NTFS does with the no-op it gets on Windows. That half is a
// claim about the platform and is stated as one, in syncdir_windows.go.
type simDisk struct {
	mu sync.Mutex

	// Contents are keyed by inode rather than by name, because a rename moves
	// a directory entry and does not touch a byte of the file. Keying by name
	// would make a reverted rename lose the data along with the name.
	durable map[int][]byte
	pending map[int][]simPage

	// trunc is a staged ftruncate: the new length, not on the platter until
	// this file is fsynced.
	trunc map[int]*int64

	faults map[int]*simFault
	errs   map[simErrKey]*simErr

	// dirLive is the directory a running process sees; dirDurable is the
	// directory on the platter. See the type comment.
	dirLive    map[string]int
	dirDurable map[string]int

	nextIno int
	crashAt *simCrashPoint

	// unlinksReachThePlatterImmediately makes every remove durable as it is
	// issued, rather than at the next syncDir. See PromoteUnlinksEarly.
	unlinksReachThePlatterImmediately bool

	// metadataIsJournalled makes every create, rename and remove durable as it
	// is issued, with no syncDir. See JournalMetadata.
	metadataIsJournalled bool

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

// simErr is one injected filesystem failure, armed for the next call to op on
// one file. A real disk returns EIO on a bad sector and ENOSPC when the
// filesystem is full, and neither is rare enough to leave untested: before
// this existed nothing in this repository ever made a filesystem call fail, so
// every error branch on the write path had never executed once.
//
// accept is how many bytes a failing WriteAt takes before it gives up, which
// is the short write. It is ignored by every other operation.
type simErr struct {
	accept int
	err    error
}

type simErrKey struct {
	ino int
	op  string
}

// simCrashPoint is a power cut armed for the end of the nth future call to op.
// See CrashAtNth.
type simCrashPoint struct {
	op string
	n  int
}

// simPowerCut is what the disk panics with when an armed crash point fires. A
// process does not run on after the power goes, so neither does the store: the
// panic is what stops the rest of the operation it was half way through.
// runUntilPowerCut in powercut_test.go recovers it.
type simPowerCut struct{ op string }

func newSimDisk() *simDisk {
	return &simDisk{
		durable:    map[int][]byte{},
		pending:    map[int][]simPage{},
		trunc:      map[int]*int64{},
		faults:     map[int]*simFault{},
		errs:       map[simErrKey]*simErr{},
		dirLive:    map[string]int{},
		dirDurable: map[string]int{},
	}
}

// FS returns the store-facing view of this disk. dir is only ever used to
// build the paths that appear in traces and error messages.
func (d *simDisk) FS(dir string) fileSystem { return simFS{disk: d, dir: dir} }

// Crash discards every byte written and not yet promoted by a Sync, every
// length change not yet fsynced, and every directory change not yet promoted
// by a syncDir. This is the power cut.
func (d *simDisk) Crash() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.crashLocked()
}

func (d *simDisk) crashLocked() {
	d.pending = map[int][]simPage{}
	d.trunc = map[int]*int64{}
	d.faults = map[int]*simFault{}
	d.errs = map[simErrKey]*simErr{}
	d.dirLive = cloneDir(d.dirDurable)
	d.collectLocked()
	d.crashes++
}

// collectLocked drops the contents of any inode no directory entry names any
// more. A file with no entry on the platter and none in the live directory is
// a file that did not survive, whatever is left of its bytes.
func (d *simDisk) collectLocked() {
	live := map[int]bool{}
	for _, ino := range d.dirDurable {
		live[ino] = true
	}
	for _, ino := range d.dirLive {
		live[ino] = true
	}
	for ino := range d.durable {
		if !live[ino] {
			delete(d.durable, ino)
			delete(d.pending, ino)
			delete(d.trunc, ino)
			delete(d.faults, ino)
		}
	}
}

func cloneDir(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// CrashAtNth arms a power cut at the END of the nth future call to op, which
// is how a test reaches a window inside a multi-step operation - between the
// rename that installs a checkpoint and the directory sync that makes it
// durable, say. op is one of create, createtrunc, open, writeat, sync,
// truncate, remove, rename, syncdir.
//
// When it fires, the disk crashes and then panics with simPowerCut, because
// the store must not run on past the point the power went.
func (d *simDisk) CrashAtNth(op string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.crashAt = &simCrashPoint{op: op, n: n}
}

// tick counts an operation and fires the armed crash point if this is the one.
// Caller holds d.mu.
func (d *simDisk) tick(op string) {
	c := d.crashAt
	if c == nil || c.op != op {
		return
	}
	c.n--
	if c.n > 0 {
		return
	}
	d.crashAt = nil
	d.crashLocked()
	panic(simPowerCut{op: op})
}

// FaultNextSync arms a fault for the next Sync on name.
func (d *simDisk) FaultNextSync(name string, f simFault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ino, ok := d.dirLive[name]; ok {
		d.faults[ino] = &f
	}
}

// FailNext arms an error for the next call to op on name. accept is how many
// bytes a failing WriteAt takes before giving up; it is ignored elsewhere.
func (d *simDisk) FailNext(name, op string, accept int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ino, ok := d.dirLive[name]; ok {
		d.errs[simErrKey{ino: ino, op: op}] = &simErr{accept: accept, err: err}
	}
}

// takeErr returns and disarms the error armed for op on ino, if any. Caller
// holds d.mu.
func (d *simDisk) takeErr(ino int, op string) *simErr {
	k := simErrKey{ino: ino, op: op}
	e := d.errs[k]
	if e != nil {
		delete(d.errs, k)
	}
	return e
}

// DurableBytes is what would be on the platter if the power went right now.
func (d *simDisk) DurableBytes(name string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	ino, ok := d.dirLive[name]
	if !ok {
		return nil
	}
	return append([]byte(nil), d.durable[ino]...)
}

// Names lists every file a running process can see, sorted.
func (d *simDisk) Names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.dirLive))
	for n := range d.dirLive {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DurableNames is what the DIRECTORY on the platter holds - what a process
// reopening after a power cut finds, which is not the same list as Names()
// until syncDir has been called.
func (d *simDisk) DurableNames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.dirDurable))
	for n := range d.dirDurable {
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
	remap := map[int]int{}
	for name, ino := range d.dirDurable {
		ni, ok := remap[ino]
		if !ok {
			out.nextIno++
			ni = out.nextIno
			remap[ino] = ni
			out.durable[ni] = append([]byte(nil), d.durable[ino]...)
		}
		out.dirDurable[name] = ni
		out.dirLive[name] = ni
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

// DirSyncs is how many times the directory was actually fsynced.
func (d *simDisk) DirSyncs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirSyncs
}

// view is what this file looks like to a running process: the platter, with
// any staged truncation applied, with everything still pending written over
// the top. Caller holds d.mu.
func (d *simDisk) view(ino int) []byte {
	out := append([]byte(nil), d.durable[ino]...)
	if t := d.trunc[ino]; t != nil {
		out = resize(out, *t)
	}
	for _, p := range d.pending[ino] {
		out = applyPage(out, p.off, p.data)
	}
	return out
}

func resize(buf []byte, size int64) []byte {
	if int64(len(buf)) > size {
		return buf[:size]
	}
	return applyPage(buf, size, nil)
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

func (d *simDisk) writeAt(ino int, p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[ino]; !ok {
		return 0, &os.PathError{Op: "writeat", Path: d.nameOf(ino), Err: os.ErrNotExist}
	}
	var failErr error
	if e := d.takeErr(ino, "writeat"); e != nil {
		// A short write takes the first accept bytes and reports the failure.
		// (*os.File).WriteAt loops internally, so a caller only ever sees this
		// when the underlying write really could not finish - ENOSPC part way
		// through a batch is the ordinary way to get here.
		if e.accept < len(p) {
			p = p[:e.accept]
		}
		failErr = e.err
	}
	// Split on absolute page boundaries, so the pieces line up with the units
	// a device would actually commit.
	total := len(p)
	for len(p) > 0 {
		n := simPageSize - int(off%simPageSize)
		if n > len(p) {
			n = len(p)
		}
		d.pending[ino] = append(d.pending[ino], simPage{off: off, data: append([]byte(nil), p[:n]...)})
		off += int64(n)
		p = p[n:]
	}
	d.tick("writeat")
	return total, failErr
}

func (d *simDisk) readAt(ino int, p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[ino]; !ok {
		return 0, &os.PathError{Op: "readat", Path: d.nameOf(ino), Err: os.ErrNotExist}
	}
	if e := d.takeErr(ino, "readat"); e != nil {
		return 0, e.err
	}
	content := d.view(ino)
	if off >= int64(len(content)) {
		return 0, io.EOF
	}
	n := copy(p, content[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// sync promotes this file's staged truncation and its pending pages, applying
// any armed fault.
//
// It always clears the pending queue, fault or no fault: the pages a fault
// drops are pages the power cut caught in flight, and they are never coming.
// A failing fsync clears it too, and that is not an oversight - Linux reports
// a writeback error once and drops the dirty pages, so a caller that retries
// gets a success over data that never landed. Modelling the friendlier version
// would flatter the store.
func (d *simDisk) sync(ino int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[ino]; !ok {
		return &os.PathError{Op: "fsync", Path: d.nameOf(ino), Err: os.ErrNotExist}
	}
	d.syncs++

	pages := d.pending[ino]
	delete(d.pending, ino)
	staged := d.trunc[ino]
	delete(d.trunc, ino)

	if e := d.takeErr(ino, "sync"); e != nil {
		d.tick("sync")
		return e.err
	}

	content := d.durable[ino]
	if staged != nil {
		content = resize(content, *staged)
	}

	order := make([]int, len(pages))
	for i := range order {
		order[i] = i
	}
	stop := len(pages)
	tear := -1

	if f := d.faults[ino]; f != nil {
		delete(d.faults, ino)
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

	for i := range stop {
		p := pages[order[i]]
		data := p.data
		if tear >= 0 && i == stop-1 && tear < len(data) {
			data = data[:tear]
		}
		content = applyPage(content, p.off, data)
	}
	d.durable[ino] = content
	d.tick("sync")
	return nil
}

// truncate stages a new length. The length is inode metadata: this process
// reads the shortened file at once, and the platter keeps the old length until
// an fsync on this descriptor promotes it.
func (d *simDisk) truncate(ino int, size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.durable[ino]; !ok {
		return &os.PathError{Op: "truncate", Path: d.nameOf(ino), Err: os.ErrNotExist}
	}
	if e := d.takeErr(ino, "truncate"); e != nil {
		return e.err
	}
	d.trunc[ino] = &size
	kept := d.pending[ino][:0]
	for _, p := range d.pending[ino] {
		if p.off+int64(len(p.data)) <= size {
			kept = append(kept, p)
		}
	}
	d.pending[ino] = kept
	d.tick("truncate")
	return nil
}

// nameOf is for error messages only. Caller holds d.mu.
func (d *simDisk) nameOf(ino int) string {
	for n, i := range d.dirLive {
		if i == ino {
			return n
		}
	}
	return "(unlinked)"
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
	if _, ok := s.disk.dirLive[name]; ok {
		return nil, &os.PathError{Op: "create", Path: name, Err: os.ErrExist}
	}
	ino := s.disk.newInoLocked()
	// A brand new directory entry, and not a durable one until syncDir -
	// unless the directory is journalled, where the entry is on the platter
	// when this returns and syncDir has nothing left to do.
	s.disk.dirLive[name] = ino
	if s.disk.metadataIsJournalled {
		s.disk.dirDurable[name] = ino
	}
	s.disk.tick("create")
	return simFile{disk: s.disk, ino: ino}, nil
}

func (s simFS) createTrunc(name string) (file, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	ino, ok := s.disk.dirLive[name]
	if !ok {
		ino = s.disk.newInoLocked()
		s.disk.dirLive[name] = ino
		if s.disk.metadataIsJournalled {
			s.disk.dirDurable[name] = ino
		}
	} else {
		zero := int64(0)
		s.disk.trunc[ino] = &zero
		delete(s.disk.pending, ino)
	}
	s.disk.tick("createtrunc")
	return simFile{disk: s.disk, ino: ino}, nil
}

func (d *simDisk) newInoLocked() int {
	d.nextIno++
	d.durable[d.nextIno] = nil
	return d.nextIno
}

func (s simFS) open(name string) (file, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	ino, ok := s.disk.dirLive[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	s.disk.tick("open")
	return simFile{disk: s.disk, ino: ino}, nil
}

func (s simFS) size(name string) (int64, error) {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	ino, ok := s.disk.dirLive[name]
	if !ok {
		return 0, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	return int64(len(s.disk.view(ino))), nil
}

func (s simFS) list() ([]string, error) { return s.disk.Names(), nil }

// remove unlinks the name. The bytes stay where they are: the entry on the
// platter still points at them until syncDir, and a power cut before that
// brings the file back.
func (s simFS) remove(name string) error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	if _, ok := s.disk.dirLive[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	delete(s.disk.dirLive, name)
	if s.disk.unlinksReachThePlatterImmediately || s.disk.metadataIsJournalled {
		delete(s.disk.dirDurable, name)
		s.disk.collectLocked()
	}
	s.disk.tick("remove")
	return nil
}

// PromoteUnlinksEarly makes every subsequent remove durable the moment it is
// issued, instead of at the next syncDir.
//
// The default model is that no directory change reaches the platter until
// syncDir, and for a DURABILITY argument that is the right assumption: it never
// lets the store take credit for a change it did not sync. For an ORDERING
// argument it is the wrong one, and wrong in a way that hides things. A real
// filesystem may write a directory block back whenever it likes, so a power cut
// part way through a sequence of unlinks can leave any subset of them durable -
// and which subset is a function of the order they were issued in. The default
// model cannot stage any of that: a crash mid-loop reverts every unlink at once,
// so the loop's order has no observable consequence.
//
// That is the exact reason reversing the unlink loop in checkpointLocked left
// the whole suite and all 240 corpus seeds green while three comments argued
// from the order. Nothing could see it. This knob is what lets a test see it,
// and it is a knob rather than the default because every other test on this
// disk is asking the durability question, where assuming less is correct.
func (d *simDisk) PromoteUnlinksEarly() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unlinksReachThePlatterImmediately = true
}

// JournalMetadata switches the directory from the POSIX rule to the NTFS one:
// a create, rename or remove is on the platter when the call returns, and no
// syncDir is needed to put it there.
//
// The default model is ext4 with data=ordered, where the durability of a name
// is the application's job - which is what syncdir_unix.go does and what
// TestANewSegmentsDirectoryEntryIsMadeDurable proves the store performs. NTFS
// makes it the filesystem's job instead: metadata operations go through the
// $LogFile transaction log before the operation is reported complete, so the
// entry is recovered from the journal after a crash. That is the documented
// reason syncdir_windows.go is a no-op rather than a gap.
//
// Until this knob existed that reasoning was a comment and nothing executed
// it. Every simulated-disk test ran the POSIX rule, so the Windows build's
// correctness rested on an argument about NTFS that no test could see. This is
// the seam that lets a test ask the question.
//
// What it proves and what it does not: with this set, a store whose syncDir
// does nothing keeps its acknowledged writes across a power cut. That is the
// store being correct GIVEN the documented NTFS contract. Whether NTFS honours
// that contract is not a question any model can answer - it is the same kind
// of residual as whether the drive honours a flush, which bench/results.md
// records as unknown for the machine it ran on.
func (d *simDisk) JournalMetadata() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.metadataIsJournalled = true
}

// rename moves a directory entry and does not touch the file's data at all,
// which is why contents are keyed by inode here. Atomic with respect to a
// reader, and not durable until syncDir.
func (s simFS) rename(from, to string) error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	ino, ok := s.disk.dirLive[from]
	if !ok {
		return &os.PathError{Op: "rename", Path: from, Err: os.ErrNotExist}
	}
	s.disk.dirLive[to] = ino
	delete(s.disk.dirLive, from)
	if s.disk.metadataIsJournalled {
		s.disk.dirDurable[to] = ino
		delete(s.disk.dirDurable, from)
	}
	s.disk.tick("rename")
	return nil
}

// syncDir makes the directory durable: every create, rename and remove issued
// since the last call is on the platter when this returns, and not before.
func (s simFS) syncDir() error {
	s.disk.mu.Lock()
	defer s.disk.mu.Unlock()
	s.disk.dirSyncs++
	s.disk.dirDurable = cloneDir(s.disk.dirLive)
	s.disk.collectLocked()
	s.disk.tick("syncdir")
	return nil
}

// simFile is one open handle. Handles on the same file share the disk's state,
// exactly as two descriptors on one inode do - including across a rename,
// which is the point of holding the inode rather than the name.
type simFile struct {
	disk *simDisk
	ino  int
}

func (f simFile) WriteAt(p []byte, off int64) (int, error) {
	return f.disk.writeAt(f.ino, p, off)
}

func (f simFile) ReadAt(p []byte, off int64) (int, error) { return f.disk.readAt(f.ino, p, off) }
func (f simFile) Sync() error                             { return f.disk.sync(f.ino) }
func (f simFile) Truncate(size int64) error               { return f.disk.truncate(f.ino, size) }
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
