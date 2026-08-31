# kvstore

An embedded key-value store in Go whose durability claim is tested rather than
asserted, by two harnesses that reach different things. One forks a child
process and kills it at a randomised point under a recorded seed; a corpus of
240 of them runs in CI on every push. The other replaces the platter instead of
killing the process, so the power can be taken away mid-write - which is the
only way to catch a store that never called `fsync` at all.

[![ci](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml/badge.svg)](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml)

## What it is

A write-ahead log, a map, and a checkpoint, in about 850 lines of Go with no
dependencies outside the standard library. (854 non-blank,
non-comment lines in the build set that ships on one platform, at the time of
writing.) `Put` returns when the record is on disk and the fsync covering it has
returned - not before.

It exists because "durable" is the easiest word in systems programming to write
and one of the harder ones to earn, and because the difference between the two
is entirely a matter of what you can demonstrate. Everything here is arranged
around being checkable by someone who does not believe you.

## Install

```sh
go get github.com/chickengamer555/kvstore
```

Go 1.27 or later. No other dependencies, at build time or at run time.

## Quickstart

```go
package main

import (
	"fmt"
	"log"

	"github.com/chickengamer555/kvstore"
)

func main() {
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
}
```

To watch the harness work, and to see what a build that loses data looks like
under it, in ascending order of how long you have to wait:

```sh
go run ./crashtest/cmd/crashrepro -seed 7960286522194355700        # one crash, ~2s
go run -tags kvearlyack ./crashtest/cmd/crashrepro -seed 7960286522194355700
go test -run PowerCut ./                                           # power cuts, instant
go test ./...                                                      # everything, ~3 min
```

The second command builds a deliberately broken store - one that acknowledges
writes while they are still in a user-space buffer - and runs the identical
harness against it. It fails, naming the keys it lost. That is most of the
reason to believe the others passing means anything.

The last one includes the 240-seed corpus and takes about three minutes. There
is no `-short` mode: a subset run is a weaker claim wearing the same green
tick.

## How the durability claim is tested

Six clauses, each with tests that map to it. `verify/kvstore.task.json` is the
machine-readable version, written before the code.

**An acknowledged write survives; an unacknowledged one need not.** This is the
whole contract, and the second half matters as much as the first. A test suite
that asserts every write survives a crash is not testing durability, it is
testing that nothing crashed. So the harness records exactly which writes were
acknowledged, insists on every one of them, and permits the single operation
that was in flight when the process died to have happened or not.

**Acknowledgement comes after the fsync, and a test can catch the store lying
about it.** `write(2)` returning means the bytes are in the page cache and a
power cut loses them. The store writes through a file interface whose test
implementation has two layers: a write lands in the pending layer, `Sync`
promotes it to the durable layer, and a simulated power cut throws the pending
layer away. So the test is `Put`, cut the power, reopen, and the value has to
be there - an assertion about the data, not about anything the store says
about itself.

That distinction is not academic. It used to be an event-ordering assertion
over strings the store appended, plus a sync counter the store incremented,
and deleting `w.f.Sync()` from `walpolicy.go` left the entire suite green -
unit tests, the 240-seed crash corpus and the negative control included. The
commit history has that build in it, and the commit after it puts the line
back. The ordering assertion is still there and is still worth having; nothing
rests on it.

**A record failing its checksum or its sequence chain ends recovery.** Each
record carries a crc32c over its own bytes, chained to the previous record's
checksum, plus a global sequence number. Recovery stops at the first record it
cannot vouch for and never skips one - closing over a gap would resurrect
writes made after the ones it lost. Tests flip a byte, truncate mid-record,
break the chain, and plant a record lifted from a different log.

**Recovery is deterministic.** Every crashed directory in the corpus is copied
before it is opened, and both copies are recovered independently and compared
byte for byte. Recovery is only deterministic if it is: the recovered state is
serialised with sorted keys, because Go randomises map iteration order and a
serialisation that walks the map differs on every run. Two other places in this
repository made the same mistake after that one was fixed - the harness's own
list of findings, and the shape tally the section below quotes - and both are
now sorted and enumerated, with tests that fail when they are not.

**Crash injection is randomised, not hand-placed.** The seed fixes the write
schedule and the kill point; hand-placed crash points only ever test the paths
the author already thought of, which are the paths that are already correct.

**The log is bounded.** Checkpointing rotates the log and deletes what it
supersedes - after the checkpoint is durable, never before. A test writes
600KB against a 32KB bound and watches the live log peak at 32,400 bytes.

## Two harnesses, and what each one reaches

**The crash corpus kills a real process.** It forks a child, kills it at a
randomised offset under a recorded seed, and checks the directory it left
behind. That reaches code paths no simulator models - real file handles, real
kernel state, whatever the operating system does with a half-finished
checkpoint - and it is the half that can surprise you.

It does **not** prove the fsync is doing its job. After `Process.Kill` the page
cache is untouched and the kernel writes out unsynced data anyway, so a store
that never called `fsync` at all sails through the whole corpus. That is not a
gap that can be closed by running more seeds. Only losing power catches it, and
nothing in user space can arrange to lose power.

It also cannot produce a torn record. For writes this small the write either
reached the kernel or it did not; a record half on the platter comes from a
page write interrupted by power loss, which is the same thing the corpus cannot
stage.

**The simulated disk takes the power away instead of the process.** The store
performs every filesystem operation through an interface (`file.go`), and the
tests supply an implementation with a pending layer and a durable layer. `Sync`
promotes pending to durable, `Crash()` discards pending, and a newly created
file is deleted unless the directory has been synced. Writes are split into
512-byte pages, so a sync can be interrupted part way through, or made to
promote a later page while an earlier one never arrives.

That reaches the three shapes the corpus provably cannot:

| shape | what it stages | test |
|---|---|---|
| power cut | everything not fsynced is gone | `TestAckedWriteSurvivesASimulatedPowerCut` |
| lost directory entry | a new segment survives with no name | `TestANewSegmentsDirectoryEntryIsMadeDurable` |
| torn page | the fsync is interrupted 30 bytes into a record | `TestATornPageLeavesEveryAcknowledgedWriteRecoverable` |
| out-of-order flush | a later page is durable, an earlier one is a hole | `TestAnOutOfOrderFlushIsNotSkippedOverOnRecovery` |

Each of the first three fails when the line it is about is removed - the log's
`Sync`, the directory's `syncDir`, the tail truncation on reopen - and each of
those builds is a commit in this repository's history rather than a claim on
this page. The fourth is declared a guard in the contract rather than a probe,
because this store never had a scanning recovery to break and inventing one to
have something to catch would be manufacturing evidence.

The simulator is not the real thing and does not pretend to be. It models a
filesystem where the application owns directory-entry durability, which is
POSIX; on Windows the platform's implementation of that call is a documented
no-op and nothing here verifies NTFS's side of the bargain. It says what the
store does. Whether the hardware honours a flush is a third question that
neither harness touches, and `bench/results.md` says so of the drive it ran on.

**Both negative controls, and one of them was cacheable.** `walpolicy_earlyack.go`
is a deliberately broken build that acknowledges writes while they are still in
a user-space buffer, and the harness is required to catch it. Under the
simulated disk it is caught every time, on every platform, because nothing
there depends on timing. Under the crash corpus the detection rate is 13 of 24
seeds on windows/amd64 and 4 of 24 on ubuntu-latest - same seeds, same build -
so that rate is reported and not asserted on.

Both controls were also invisible to Go's test cache: the broken build's source
is excluded from both test binaries by its build tag, so `go test ./...` would
serve a cached pass after it had been disarmed. Each control now reads that
file, which is what registers it as an input. A fresh checkout - CI - was never
affected; a local run was.

### What was measured, rather than assumed

"240 seeds" says how many times the harness ran, not how many interesting
places it interrupted. Those are different numbers and the second one is the
honest one, so there is a command that prints it:

```sh
go run ./crashtest/cmd/crashrepro -corpus-shapes
```

Every shape it can classify gets a row whether it happened or not, because the
zero rows are the ones this section is about. On the author's Windows machine:

| | 240 seeds, windows/amd64 |
|---|---|
| killed while writing a checkpoint | 0 |
| checkpoint rejected on its checksum | 0 |
| killed between rotating the log and deleting the old segments | 0 |
| killed before the superseded segments were deleted | 0 |
| recovery stopped at a damaged record | 0 |
| log ended on a record boundary | 240 |
| **classified** | **240** |
| recovered through a checkpoint | 218 |
| seeds that failed | 0 |

**Corpus reach is platform-dependent, and this table is the worse of the two.**
The first CI run on `ubuntu-latest` classified 40 seeds like this:

| | 40 seeds, ubuntu-latest |
|---|---|
| killed while writing a checkpoint | 1 |
| killed between rotating the log and deleting the old segments | 5 |
| log ended on a record boundary | 34 |

Same corpus, same code, five of forty landing in a window that zero of two
hundred and forty reached here. `SIGKILL` and `TerminateProcess` preempt
differently and Linux is evidently the richer of the two. That is one run of a
40-seed subset against one run of the full 240, so the two are not directly
comparable and neither is a distribution you should trust to three figures -
but the direction is not ambiguous, and the Windows figure is not the general
figure. CI's Linux column is the authoritative one; this machine's is
informative.

Two things follow from the zeros that remain.

A process kill does not produce torn records for writes this small: the write
either reached the kernel or it did not. That shape is now staged directly, by
interrupting an fsync part way through a page under the simulated disk, and by
hand-built logs in `record_test.go`. It is no longer a hole.

And the checkpoint windows are reached rarely enough on Windows that the corpus
cannot be relied on to hit them there. They are covered explicitly as well, by
`TestPartialCheckpointIsIgnored` and
`TestRecoveryIgnoresRecordsTheCheckpointAlreadyCovers`, which reconstruct each
window by hand. The randomised corpus is what would catch a window nobody
thought of; the hand-built tests cover the two that are known. Neither is a
substitute for the other, and Linux reaching them 6 times in 40 is the corpus
doing its job.

The corpus itself is generated, not chosen: `crashtest/corpus.txt` is splitmix64
from a stated origin, and `TestCorpusSizeFloor` recomputes it and compares. A
seed that started failing cannot be quietly deleted from the file.

### Reproducing a failure

Two different claims, and only one of them is exact.

**Re-running a seed is not exact.** The seed fixes the schedule and the kill
point in acknowledgements, but it cannot fix the instruction the signal lands
on - that belongs to the scheduler, and it is the whole reason the test is
worth running. Measured against the deliberately broken build, a failing seed
fails again on roughly half its re-runs.

```sh
go run ./crashtest/cmd/crashrepro -seed <the seed the test printed>
```

**Replaying a preserved crash is exact.** When a seed fails, the harness copies
the directory the dead process left behind, and CI uploads it as an artifact.
Replaying those bytes reproduces the identical verdict and the identical
findings, in the identical order, every time the directory exists - which is
what you actually want when you are trying to fix something.

That was not true until recently and is worth saying so. The findings were
built by ranging over a map, so three replays of one preserved directory gave
the same failures in three different orders, and the test that was supposed to
catch it compared sorted categories and a count rather than the strings.
`TestReplayedFindingsAreInAStableOrder` compares them verbatim now.

```sh
go run ./crashtest/cmd/crashrepro -replay crash-failures/seed-<n>
```

## Which platform proves what

The durability claims here are POSIX claims, and CI runs on `ubuntu-latest`
because that is where they mean something. The Windows job runs the same suite
and is informative rather than authoritative.

| | Linux (CI) | Windows (author's machine) |
|---|---|---|
| acknowledgement after the log's fsync | run, green | run, green |
| checksum and sequence chain end recovery | run, green | run, green |
| deterministic recovery | run, green | run, green |
| bounded log under sustained writes | run, green | run, green |
| randomised crash corpus, 240 seeds | run, green - richer distribution, see above | run, green |
| **directory fsync on log creation** | **run, green - `DirSync:true`, first execution anywhere** | **not applicable - see below** |
| race detector | run, clean | cannot run - no C toolchain here |
| negative control detection rate | 4 of 24 seeds | 13 of 24 seeds |

There is CI history now, and the first run was **red**, on the last row. The
negative control's floor was 6 of 24, set from the Windows measurement, and
Linux caught the same broken build on the same seeds 4 times. The guard was
right to fire; the number was wrong, and a rate that moves by a factor of three
between platforms is not a property of this store. The rate is now reported
rather than asserted, and the load-bearing negative control moved to the
simulated disk where it is deterministic. That is a real weakening of one
assertion and a real strengthening of another, and both halves are in the
commit history.

The rest of that run is the first evidence any of this has on Linux: the race
detector ran clean for the first time anywhere, and `DirSync` reported true,
which is the first time the POSIX directory-fsync path has executed at all.

Read "run, green" as one CI run, not as a track record. The badge at the top of
this file is the live answer.

The directory-fsync row is the one that only Linux can settle, and it is the
reason CI exists here rather than being decoration.

Creating a file and fsyncing it makes the file's contents durable. It does not
make the directory entry that names the file durable: that entry is the parent
directory's own metadata, and on ext4 it can still be in the journal when the
power goes. The file survives with no name, which is the same as not surviving.
So on POSIX the store opens the containing directory and calls `fsync(2)` on
that descriptor too, whenever it creates a log segment or installs a
checkpoint.

On Windows there is no such call, and none is needed - which is a different
statement from "not possible". NTFS journals metadata operations through
`$LogFile`: the directory entry is made durable by the filesystem rather than
by the application, so the no-op is correct there. What is not acceptable is
staying quiet about the difference, so `kvstore.Platform()` reports which of
the two this build is, in words, at run time - and
`TestPlatformReportsItsDirectorySyncGuaranteeHonestly` asserts the store always
reaches that decision and that what it says about itself matches the build it
is, whichever platform it is on.

That test is named for exactly what it checks, because the previous name
promised more. It does not establish that the directory sync makes anything
durable - an emitted event cannot - and CI reporting it as PASS on Windows over
a log line saying nothing had been verified was the wrong shape.
`TestANewSegmentsDirectoryEntryIsMadeDurable` is what establishes the sync
happens: the simulated disk deletes a file whose directory was never synced, so
removing `syncDir` from `createSegment` loses the whole log.

Nothing on the Windows side of that row - whether NTFS really makes the entry
durable without the application asking - has been verified by anything here.

## Benchmarks

Full output, with the machine and filesystem it came from, is in
[`bench/results.md`](bench/results.md). Regenerate it with one command:

```sh
go run ./bench -machine "..." -kernel "..." -fs "..." -flush-honoured "..."
```

The harness prints markdown, so the file is exactly what the harness produced;
nothing is transcribed by hand. On an i7-8700K with an NVMe SSD and NTFS:

| workload | ops/sec |
|---|---:|
| `Put`, one fsync per call | 442 |
| `PutBatch` of 100, one fsync per batch | 36,931 |
| `Get` against a warm store | 1,065,551 |
| recovery, 100,000 records / 15.4 MiB of log | 144 ms |

Those are medians of three on a desktop under ordinary load, and they move
between runs - the write figures by a few percent, the read figure by as much
as a factor of two, because it is a map lookup and is dominated by whatever
else the machine is doing. Treat them as an order of magnitude, and re-run the
harness rather than trusting the table.

**442 writes per second is the honest number.** The batched figure buys its
eighty-fold improvement by moving the unit of acknowledgement from the record
to the batch: nothing in a `PutBatch` is durable until the call returns, and a
crash in the middle can leave any prefix of it. That is a real weakening of the
guarantee, not a free win, which is why the unbatched figure is the one quoted
first.

**Where this loses.** Against any real storage engine, on throughput, by one to
three orders of magnitude. RocksDB, LMDB and SQLite in WAL mode all default to
not fsyncing every write, and even configured to do so (`synchronous=FULL` and
friends) they win on batching, on threading, and on being storage engines
rather than eight hundred lines of demonstration. Writes here are serialised
behind one mutex, so the core count is irrelevant to the write path. The read figure has no disk in it at all - the
live key set is in memory - and should be read as a measurement of Go's map.

Those numbers are from Windows. The durability claims they sit beside are
proven on Linux. Re-run the harness there before quoting any of it as a Linux
figure; CI does exactly that on every push and prints the result.

## Limitations

Deliberate, and none of them are on a roadmap.

- **The whole live key set is in memory.** A store larger than RAM is not a
  store this can open.
- **One process at a time.** There is no file locking, no multi-process access,
  and no detection of a second process opening the same directory.
- **No transactions, iterators, range scans or secondary indexes.** `Put`,
  `Get`, `Delete`, `PutBatch`. That is the whole API.
- **No compression, block cache or bloom filters.**
- **A checkpoint is bounded by the live key set, not by the write volume.** The
  log is bounded; the checkpoint is as large as your data.
- **The oracle and the implementation have the same author.** A model check
  catches divergence from my own understanding of correctness, not from a
  specification. That is weaker evidence than an external conformance suite
  like `toml-test`, and no amount of seeds fixes it.
- **The simulated disk is a model, and I wrote it.** It stages a power cut
  faithfully enough to catch a missing `fsync`, a missing `syncDir` and a
  missing truncation, and each of those was checked by removing the line and
  watching the test fail. It does not model rename and remove reverting, it
  assumes a filesystem where the application owns directory-entry durability,
  and it says nothing at all about whether a drive honours a flush. Every
  simulator is an argument about which details matter; this one's are written
  down at the top of `simdisk_test.go`.

## Design

`record.go` has the on-disk layout and the reasoning behind it. Two things are
load-bearing:

The checksum covers the length field. It has to - otherwise a crash that
scribbles the length turns a torn tail into a plausible record of the wrong
size, and everything after it decodes as garbage that passes its own checks.
The cost is that a corrupt length is only caught after reading that many bytes,
which is why there is a hard ceiling above the length check: a garbage prefix
claiming 4GB must be treated as the end of the log, not as an allocation.

The checksum is chained - each record's crc is seeded with the previous
record's crc rather than with zero. That is what makes the sequence number an
actual chain: a record that is internally perfect but was written after a
different predecessor fails, so a record lifted from elsewhere in the log
cannot be accepted at the wrong position.

`checkpoint.go` has the four-step ordering that makes checkpointing safe, and
the invariant behind it: a segment is only ever removed after the checkpoint
that supersedes it is on disk, so there is no instant at which neither holds
the data.

## Verification

Every case in `verify/kvstore.task.json` was observed failing before it passed,
with one declared exception, and the record is committed in
`.general-harness/redproof.json` - the test name that failed, when, and against
which version of the contract. A test that has never been seen to fail has not
been shown to be wired to anything.

The stronger version of that evidence is in the git history rather than in the
JSON, because a file the author's own tooling wrote is weaker proof than a
build a stranger can check out and run. Several commits here are deliberately
broken and are labelled `(red)` in their subject: the log's fsync deleted, the
directory's fsync deleted, the torn-tail truncation deleted, the negative
control disarmed, and the honest write path replaced with the buffering one so
the crash corpus can be watched catching a store that really loses data. Each
is followed by the commit that restores it. `git checkout <sha> && go test ./...`
is the check.

The one exception is declared in the contract as a guard rather than a probe:
the out-of-order flush test has no honest red proof, because this store never
had a scanning recovery to break. Twelve of the cases carry proofs recorded
against an earlier version of the contract, which `general-verify` reports as a
warning; the contract gained a unit this turn and those cases were not
re-observed.

## Licence

MIT. See [LICENSE](LICENSE).
