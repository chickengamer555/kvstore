# kvstore

An embedded key-value store in Go whose durability claim is tested rather than
asserted: a harness forks a child process, kills it at a randomised point under
a recorded seed, reopens the store and checks that every acknowledged write
survived. 240 seeds run in CI on every push.

[![ci](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml/badge.svg)](https://github.com/chickengamer555/kvstore/actions/workflows/ci.yml)

## What it is

A write-ahead log, a map, and a checkpoint, in about 800 lines of Go with no
dependencies outside the standard library. `Put` returns when the record is on
disk and the fsync covering it has returned - not before.

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

To watch the crash harness work, and to see what a build that loses data looks
like under the same harness:

```sh
go test ./...                                                    # everything, corpus included
go run ./crashtest/cmd/crashrepro -seed 7960286522194355700      # one seed, verbosely
go run -tags kvearlyack ./crashtest/cmd/crashrepro -seed 7960286522194355700
```

The third command builds a deliberately broken store - one that acknowledges
writes while they are still in a user-space buffer - and runs the identical
harness against it. It fails, loudly, naming the keys it lost. That is the only
reason to believe the first two commands passing means anything.

## How the durability claim is tested

Six clauses, each with tests that map to it. `verify/kvstore.task.json` is the
machine-readable version, written before the code.

**An acknowledged write survives; an unacknowledged one need not.** This is the
whole contract, and the second half matters as much as the first. A test suite
that asserts every write survives a crash is not testing durability, it is
testing that nothing crashed. So the harness records exactly which writes were
acknowledged, insists on every one of them, and permits the single operation
that was in flight when the process died to have happened or not.

**Acknowledgement comes after the fsync.** `write(2)` returning means the bytes
are in the page cache and a power cut loses them. A trace assertion records
`write-return`, `sync-start`, `sync-return` and `ack` as they happen and
insists on that order.

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
serialisation that walks the map differs on every run.

**Crash injection is randomised, not hand-placed.** The seed fixes the write
schedule and the kill point; hand-placed crash points only ever test the paths
the author already thought of, which are the paths that are already correct.

**The log is bounded.** Checkpointing rotates the log and deletes what it
supersedes - after the checkpoint is durable, never before. A test writes
600KB against a 32KB bound and watches the live log peak at 32,400 bytes.

## What the crash corpus proves, and what it does not

It proves the store survives its process dying at an arbitrary instruction. It
does **not** prove the fsync is doing its job, and the distinction is worth
being blunt about because it would be easy to imply otherwise.

After `Process.Kill` the page cache is untouched and the kernel writes out
unsynced data anyway. A store that never called `fsync` at all would sail
through this corpus. Only losing power catches that, and nothing in user space
can arrange to lose power. The fsync's place in the ordering is established by
the trace assertion and by reading `walpolicy.go`; the corpus establishes
something different and also necessary, which is that death at any point leaves
a directory that recovers correctly.

This is why the deliberately broken build in `walpolicy_earlyack.go` buffers in
user space rather than simply dropping the fsync. Dropping the fsync would not
have been caught, and a negative control that the harness cannot catch is not a
control.

### What was measured, rather than assumed

Running the corpus on the author's Windows machine and counting where the kills
actually landed:

| | 240 seeds, windows/amd64 |
|---|---|
| recovery stopped at a torn record | 0 |
| killed while writing a checkpoint | 1 |
| killed between rotating the log and deleting the old segments | 0 |
| recovered through a checkpoint | 218 |

Two things follow. A process kill does not produce torn records for writes this
small - the write either happened or it did not - so the torn-record path is
covered by hand-built logs in `record_test.go` rather than by the corpus. And
the two narrow checkpoint windows are reached rarely enough on Windows that
they are covered explicitly, by `TestPartialCheckpointIsIgnored` and
`TestRecoveryIgnoresRecordsTheCheckpointAlreadyCovers`, which reconstruct them
by hand. The randomised corpus is what would catch a window nobody thought of;
the hand-built tests cover the ones that are known and rarely hit. Neither is a
substitute for the other, and `TestCrashCorpusRecoveryIsDeterministic` prints
the same distribution on every CI run so this table can be checked rather than
believed.

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
findings every time, for as long as the directory exists - which is what you
actually want when you are trying to fix something.

```sh
go run ./crashtest/cmd/crashrepro -replay crash-failures/seed-<n>
```

## Which platform proves what

The durability claims here are POSIX claims, and CI runs on `ubuntu-latest`
because that is where they mean something. The Windows job runs the same suite
and is informative rather than authoritative.

| | Linux | Windows |
|---|---|---|
| acknowledgement after the log's fsync | proven | proven |
| checksum and sequence chain end recovery | proven | proven |
| deterministic recovery | proven | proven |
| bounded log under sustained writes | proven | proven |
| randomised crash corpus, 240 seeds | proven | proven, different kernel behaviour |
| **directory fsync on log creation** | **proven** | **not applicable - see below** |

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
the two this build is, in words, at run time - and `TestDirFsyncOnLogCreate`
asserts the store always reaches that decision and always records the answer,
whichever platform it is on.

Nothing on the Windows side of that row has been verified by anything here.

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
and the record of that is committed in `.general-harness/redproof.json` - the
test name that failed, when, and against which version of the contract. A test
that has never been seen to fail has not been shown to be wired to anything.

## Licence

MIT. See [LICENSE](LICENSE).
