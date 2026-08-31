# kvstore

An embedded key-value store in Go whose durability claim is tested rather than
asserted, by two harnesses that reach different things. One forks a child
process and kills it at a randomised point under a recorded seed; a corpus of
240 of them runs in CI on every push. The other replaces the platter instead of
killing the process, so the power can be taken away mid-write - which is the
only way to catch a store that never called `fsync` at all.

[![ci](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml/badge.svg)](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml)

Read a green tick as one CI run, not as a track record. There were three runs
at the time of writing, and the first of them was **red** - a negative-control
threshold set from a Windows measurement meeting Linux reality. What happened
next is in [docs/verification.md](docs/verification.md), and the red run is
still in the history rather than squashed out of it.

## What it is

A write-ahead log, a map, and a checkpoint, in 860 non-blank, non-comment
lines of Go - the build set that ships on one platform, counted at the time of
writing - with no dependencies outside the standard library. `Put` returns when
the record is on disk and the fsync covering it has returned, not before.

The measurements behind everything here - which platform settles which claim,
what the crash corpus was measured to actually reach, the benchmark numbers and
where they lose, how to reproduce a failure, and every line that has been
deleted to watch a test go red - are in
[**docs/verification.md**](docs/verification.md).

## Install

```sh
go get github.com/chickengamer555/kvstore
```

Go 1.27 or later. No other dependencies, at build time or at run time.

## Quickstart

```go
s, err := kvstore.Open("mystore")
if err != nil {
	log.Fatal(err)
}
defer s.Close()

// Returns after the fsync. If the machine dies on the next instruction,
// this key is still there.
if err := s.Put("greeting", []byte("hello")); err != nil {
	log.Fatal(err)
}

v, ok := s.Get("greeting")
fmt.Println(string(v), ok) // hello true

// What this build actually promises, on the platform it was compiled for.
fmt.Printf("%+v\n", kvstore.Platform())
```

To watch the harness work, and to see what a build that loses data looks like
under it, in ascending order of how long you have to wait:

```sh
go run ./crashtest/cmd/crashrepro -seed 7960286522194355700        # one crash, ~2s
go run -tags kvearlyack ./crashtest/cmd/crashrepro -seed 7960286522194355700
go test -count=1 -run 'SimDisk|PowerCut|Torn|OutOfOrder|DirectoryEntry|Checkpoint|Fail|ShortWrite|Lifted' ./
go test -count=1 ./...                                            # everything, ~3 min
```

The second builds a deliberately broken store - one that acknowledges writes
while they are still in a user-space buffer - and runs the identical harness
against it. On that seed it fails, naming the keys it lost.

**That seed is the best case, not the average one.** Across the first 24 seeds
of the corpus the same broken build was caught 13 times on Windows and **4 times
on ubuntu-latest** - the platform this README calls authoritative for every
durability claim in it. So the corpus misses a store with no `fsync` in it
twenty times out of twenty-four on the platform that matters, which is not a
tuning problem: after `Process.Kill` the page cache is intact and the kernel
writes the data out anyway. The reason to believe a green corpus run means
anything is the *simulated disk*, where the same broken build fails
deterministically on every platform. The corpus is evidence about paths, not
about `fsync`.

`-count=1` is not decoration. Without it Go serves a cached pass: on a warm
checkout `go test ./...` comes back green in well under a second having run
nothing, and a reader could reasonably think the 240-seed corpus had just gone
by. There is no `-short` mode either; a subset run is a weaker claim wearing
the same green tick.

## What is claimed, and what checks it

Six clauses, each with tests that map to it. `verify/kvstore.task.json` is the
machine-readable version, written before the code.

**An acknowledged write survives; an unacknowledged one need not.** The second
half matters as much as the first: a suite that asserts every write survives a
crash is testing that nothing crashed. The harness records exactly which writes
were acknowledged, insists on every one, and permits the operation that was in
flight when the process died to have happened or not.

**Acknowledgement comes after the fsync, and a test can catch the store lying
about it.** `write(2)` returning means the bytes are in the page cache and a
power cut loses them, so the evidence cannot be an event the store emitted. It
is the data: `Put`, cut the power, reopen, and the value has to be there. Until
the simulated disk existed the evidence *was* an emitted event, and deleting
`w.f.Sync()` left the entire suite green - the 240-seed corpus included. That
build is a commit here, and the commit after it puts the line back.

**The disk is allowed to say no.** A commit that fails part way leaves bytes
the log cannot vouch for with the write offset already past them, so the
*next* record lands beyond the point recovery stops at: fsynced, acknowledged,
unreachable for ever. That was a real bug, found by the first test in this
repository that ever made a filesystem call fail. A segment now refuses every
write after a commit on it has failed.

That closed half of it. The other half was found by a reviewer walking one step
further along the same sequence: the *store* could still checkpoint, which
rotates onto a fresh segment and leaves the poisoned one underneath it, and the
next `Open` then deletes the successor the store had been acknowledging writes
into. Two acknowledged keys gone on a healthy machine with no crash anywhere in
the sequence. `checkpointLocked` now refuses to rotate off a segment recovery
cannot vouch for, and `TestACheckpointNeverRotatesAwayFromAFailedSegment` is the
test that was red first.

**A record failing its checksum or its sequence chain ends recovery.** crc32c
over each record's own bytes, chained to the previous record's checksum within
its segment, plus a global sequence number. Recovery stops at the first record
it cannot vouch for and never skips one. The chain restarts at each segment
boundary, for a reason and with a consequence, and both are in
[docs/verification.md](docs/verification.md).

**Recovery is deterministic.** Every crashed directory in the corpus is copied
and both copies recovered independently, then compared byte for byte - which is
only true if the serialisation is sorted, because Go randomises map iteration.
Two other places here made the same mistake after the first was fixed.

**The log is bounded.** Checkpointing rotates the log and deletes what it
supersedes, after the checkpoint is durable and never before. One test writes
600KB against a 32KB bound and watches the live log peak at 32,400 bytes;
another takes the power away afterwards, because a deletion that was made is
not the same as a deletion that was made durable.

## Two harnesses, and what each one reaches

**The crash corpus kills a real process**, at a randomised offset under a
recorded seed, and checks the directory it left behind. That reaches paths no
simulator models - real handles, real kernel state, whatever the OS does with a
half-finished checkpoint.

It does **not** prove the fsync is doing its job. After `Process.Kill` the page
cache is untouched and the kernel writes unsynced data out anyway, so a store
that never called `fsync` sails through the whole corpus. More seeds cannot
close that; only losing power catches it, and nothing in user space can arrange
to lose power. It cannot produce a torn record either.

**The simulated disk takes the power away instead of the process.** File
contents have a pending and a durable layer; the directory has a live and a
durable one. `Sync` promotes a file's pages, `syncDir` promotes the directory's
creates, renames and unlinks, and `Crash()` discards whatever has not been
promoted. Writes split into 512-byte pages, calls can be made to fail, and the
power can go in the middle of a multi-step operation.

| shape the corpus cannot stage | test |
|---|---|
| power cut: everything not fsynced is gone | `TestAckedWriteSurvivesASimulatedPowerCut` |
| a new segment survives with no directory entry | `TestANewSegmentsDirectoryEntryIsMadeDurable` |
| an fsync interrupted 30 bytes into a record | `TestATornPageLeavesEveryAcknowledgedWriteRecoverable` |
| a later page durable, an earlier one a hole | `TestAnOutOfOrderFlushIsNotSkippedOverOnRecovery` |
| a superseded segment comes back: the unlink reverted | `TestACheckpointStillBoundsTheLogAfterAPowerCut` |
| the installed checkpoint is not there: the rename reverted | `TestTheCheckpointIsDurableAsSoonAsItIsInstalled` |
| fsync returns EIO; a write takes ten bytes and stops | `TestAFailedSyncNeverProducesAnAcknowledgement` |
| a segment that stopped accepting writes, reopened | `TestAStoreWhoseSegmentFailedReopensAndResumes` |
| a checkpoint asked for while the live segment is poisoned | `TestACheckpointNeverRotatesAwayFromAFailedSegment` |
| the power out at each of the ten calls a checkpoint makes | `TestAPowerCutAnywhereInTheCheckpointPathLosesNothing` |

All but one fail when the line they are about is removed, and those builds are
commits here rather than claims on this page. The exception is the out-of-order
flush, declared a guard rather than a probe in the contract, because this store
never had a scanning recovery to break and inventing one would be manufacturing
evidence.

The simulator is not the real thing. It models a filesystem where the
application owns directory-entry durability, which is POSIX; on Windows that
call is a documented no-op and nothing here verifies NTFS's side of the
bargain. Whether the hardware honours a flush is a third question neither
harness touches, and `bench/results.md` says so of the drive it ran on.

## Speed, and where this loses

**442 writes per second** with one fsync per acknowledgement, on an i7-8700K
with an NVMe SSD and NTFS, median of three. That is the honest number. Batched
with `PutBatch` it is 36,931/sec, and that figure buys its eighty-fold
improvement by moving the unit of acknowledgement from the record to the batch -
a real weakening of the guarantee, not a free win. Reads are a map lookup with
no disk in them.

**Where this loses: against any real storage engine, on throughput, by one to
three orders of magnitude.** RocksDB, LMDB and SQLite in WAL mode all default to
not fsyncing every write, and configured to do so they still win on batching, on
threading, and on being storage engines rather than eight hundred lines of
demonstration. Writes here are serialised behind one mutex, so core count does
not help the write path. The claim in this repository is durability, not speed.

Those numbers are from Windows; the durability claims beside them are proven on
Linux. Re-run the harness before quoting any of it - `bench/results.md` is
exactly what the harness printed, and the command is in
[docs/verification.md](docs/verification.md).

## Limitations

Deliberate, and none of them are on a roadmap.

- **The whole live key set is in memory**, and a checkpoint is as large as your
  data. The log is bounded; the store is not.
- **One process at a time.** No file locking, and no detection of a second
  process opening the same directory.
- **No transactions, iterators, range scans, secondary indexes, compression,
  block cache or bloom filters.** `Put`, `Get`, `Delete`, `PutBatch` is the
  whole API.
- **This is a crash model, not a threat model.** crc32c is a checksum, not a
  MAC: it detects damage and cannot detect substitution by anything that can
  write to the store's directory. A whole log segment lifted from a different
  store at a matching boundary is accepted, and there is a test that does it
  and records the result.
- **The oracle and the implementation have the same author.** A model check
  catches divergence from the author's understanding of correctness, not from a
  specification - weaker evidence than an external suite like `toml-test`, and
  no number of seeds fixes it.
- **The simulated disk is a model, not a disk**, written by the same author as
  the store it judges. What it does not model is three specific things, listed
  at the top of `simdisk_test.go`.

## Licence

MIT. See [LICENSE](LICENSE).
