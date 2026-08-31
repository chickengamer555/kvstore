# Verification

The long form of the evidence behind [the README](../README.md): what the two
harnesses were measured to reach, which platform settles which claim, how to
reproduce a failure, where the benchmark numbers came from, and which lines in
this repository have been observed to be load-bearing by deleting them.

Nothing here is a stronger claim than the README makes. It is the same claims
with the measurements attached.

That list of load-bearing lines is not the complete list, and saying so is what
the last section on this page is for: a reviewer deleted three lines this page
never nominated and the whole suite stayed green on all three, one of which was
the premise this page argues from when it retracts a claim.

## What the corpus was measured to reach

"240 seeds" says how many times the harness ran, not how many interesting
places it interrupted. Those are different numbers and the second one is the
honest one, so there is a command that prints it:

```sh
go run ./crashtest/cmd/crashrepro -corpus-shapes
```

Every shape it can classify gets a row whether it happened or not, because the
zero rows are the ones this section is about.

**This table is one run's output, not a property of the store.** It has been
run five times on the author's Windows machine at the time of writing: four
produced exactly the numbers below, and one produced `1` in the first row and
`239` in the last. So the zeros are rare rather than structural, and a reader
who runs the command and sees a `1` has not found a discrepancy - they have
found the fifth run.

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
The first CI run on `ubuntu-latest` classified 40 seeds like this. It predates
the change that made the tally enumerate every shape, so the rows that did not
occur are absent rather than zero:

| | 40 seeds, ubuntu-latest |
|---|---|
| killed while writing a checkpoint | 1 |
| killed between rotating the log and deleting the old segments | 5 |
| log ended on a record boundary | 34 |

Same corpus, same code, six of forty landing in windows that a run of two
hundred and forty here reached once or not at all. `SIGKILL` and
`TerminateProcess` preempt differently and Linux is evidently the richer of the
two. That is one run of a 40-seed subset against five runs of the full 240, so
the two are not directly comparable and neither is a distribution you should
trust to three figures - but the direction is not ambiguous, and the Windows
figure is not the general figure. CI's Linux column is the authoritative one;
this machine's is informative.

Two things follow.

A process kill does not produce torn records for writes this small: the write
either reached the kernel or it did not. That shape is staged directly instead,
by interrupting an fsync part way through a page under the simulated disk, and
by hand-built logs in `record_test.go`.

And the checkpoint windows are reached rarely enough on Windows that the corpus
cannot be relied on to hit them there - **rarely, which is not the same as never,
and the paragraph above used to read as though it were.** They are covered
explicitly as well: `TestPartialCheckpointIsIgnored` and
`TestRecoveryIgnoresRecordsTheCheckpointAlreadyCovers` reconstruct two windows
by hand, and `TestTheCheckpointIsDurableAsSoonAsItIsInstalled` takes the power
out inside a third. The randomised corpus is what would catch a window nobody
thought of; the hand-built tests cover the ones that are known. Neither is a
substitute for the other.

The corpus itself is generated, not chosen: `crashtest/corpus.txt` is
splitmix64 from a stated origin, and `TestCorpusSizeFloor` recomputes it and
compares. A seed that started failing cannot be quietly deleted from the file.

## The test cache, and one thing I could not reproduce

Go caches test results, and both negative controls in this repository shell out
to a build the test binary does not otherwise depend on - so the file that makes
the control a control is not an input to the package under test, and `go test`
will serve a stale pass after it has been disarmed. That was found the hard way
and each control now reads the source it depends on, which is what registers it.

Two things changed this turn. The harness gate is `go test -count=1 ./...`
rather than `go test ./...`, matching what CI has always used; a gate that can
pass from cache is a gate nobody should trust. And `buildBrokenChild` reads
`crashtest/cmd/crashrepro/main.go` as well as `walpolicy_earlyack.go`, because
it registered the file the build tag swaps in and not the program the tag is
applied to.

The second of those I have not been able to demonstrate. The report I was
working from is that modifying `main.go` and re-running `go test ./crashtest/`
returned `(cached)`. After the change I ran that command four times on this
machine - once cold with `-count=1`, once warm, once with a comment line
appended to `main.go`, and once more with nothing changed - and every run
executed in full, between 152 and 155 seconds, with none reporting `(cached)`.
So this package does not appear to be cacheable here at all at the moment, and I
could not stage the state the fix is for.

The fix is still right: making the cache key depend on the file the test
compiles is what stops a stale pass, whether or not I can currently produce one.
But it is a precaution I have not watched fire, which is a weaker thing than
everything else on this page, and it is written down here rather than left to
look like a verified result.

## Reproducing a failure

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

Read "run, green" as one CI run, not as a track record. The badge in the README
is the live answer.

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
which is the first time the POSIX directory-fsync path executed at all.

### The directory fsync, which only Linux can settle

Creating a file and fsyncing it makes the file's contents durable. It does not
make the directory entry that names the file durable: that entry is the parent
directory's own metadata, and on ext4 it can still be in the journal when the
power goes. The file survives with no name, which is the same as not surviving.
So on POSIX the store opens the containing directory and calls `fsync(2)` on
that descriptor too. It does that in three places, and each of the three fails
a test when it is deleted:

| call | test that fails without it |
|---|---|
| after creating a log segment (`wal.go`) | `TestANewSegmentsDirectoryEntryIsMadeDurable` |
| after the rename that installs a checkpoint (`checkpoint.go`) | `TestTheCheckpointIsDurableAsSoonAsItIsInstalled` |
| after unlinking the segments a checkpoint superseded (`checkpoint.go`) | `TestACheckpointStillBoundsTheLogAfterAPowerCut` |

There used to be a fourth, between writing the checkpoint's temporary file and
renaming it. It was removed, because one directory fsync after the rename makes
the whole of the directory state durable - the create included - so no test
could be written that failed when it was gone. A line the page credits and
nothing checks is the thing this repository exists to refuse.

One directory sync in `store.go`, after removing segments recovery found
unreachable, is **not** covered: deleting it leaves the suite green. It is
cleanup rather than correctness - those segments hold nothing recovery can
reach - and it is listed here rather than left for someone to find.

On Windows there is no such call, and none is needed - which is a different
statement from "not possible". NTFS journals metadata operations through
`$LogFile`: the directory entry is made durable by the filesystem rather than by
the application, so the no-op is correct there. What is not acceptable is
staying quiet about the difference, so `kvstore.Platform()` reports which of the
two this build is, in words, at run time, and
`TestPlatformReportsItsDirectorySyncGuaranteeHonestly` asserts that what the
store says about itself matches the build it is.

That test is named for exactly what it checks, because the previous name
promised more. It does not establish that the directory sync makes anything
durable - an emitted event cannot. Nothing on the Windows side of that row -
whether NTFS really makes the entry durable without the application asking -
has been verified by anything here.

## Benchmarks

Full output, with the machine and filesystem it came from, is in
[`bench/results.md`](../bench/results.md). Regenerate it with one command:

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
between runs - the write figures by a few percent, the read figure by as much as
a factor of two, because it is a map lookup and is dominated by whatever else
the machine is doing. Treat them as an order of magnitude, and re-run the
harness rather than trusting the table.

**442 writes per second is the honest number.** The batched figure buys its
eighty-fold improvement by moving the unit of acknowledgement from the record to
the batch: nothing in a `PutBatch` is durable until the call returns, and a
crash in the middle can leave any prefix of it. That is a real weakening of the
guarantee, not a free win, which is why the unbatched figure is quoted first.

**Where this loses.** Against any real storage engine, on throughput, by one to
three orders of magnitude. RocksDB, LMDB and SQLite in WAL mode all default to
not fsyncing every write, and even configured to do so (`synchronous=FULL` and
friends) they win on batching, on threading, and on being storage engines rather
than eight hundred lines of demonstration. Writes here are serialised behind one
mutex, so the core count is irrelevant to the write path. The read figure has no
disk in it at all - the live key set is in memory - and should be read as a
measurement of Go's map.

Those numbers are from Windows. The durability claims they sit beside are proven
on Linux. Re-run the harness there before quoting any of it as a Linux figure;
CI does exactly that on every push and prints the result.

## Design notes

`record.go` has the on-disk layout and the reasoning behind it. Two things are
load-bearing:

**The checksum covers the length field.** It has to - otherwise a crash that
scribbles the length turns a torn tail into a plausible record of the wrong
size, and everything after it decodes as garbage that passes its own checks. The
cost is that a corrupt length is only caught after reading that many bytes,
which is why there is a hard ceiling above the length check: a garbage prefix
claiming 4GB must be treated as the end of the log, not as an allocation.

**The checksum is chained** - each record's crc is seeded with the previous
record's crc rather than with zero - **within a segment**. That is what makes
the sequence number an actual chain: a record that is internally perfect but was
written after a different predecessor fails, so a record lifted from elsewhere
in the same log cannot be accepted at the wrong position.

The chain restarts at zero at each segment boundary, because a checkpoint may
have deleted the segment whose last record the first record would otherwise
chain to. Across a boundary only the sequence number and the abutment check
carry, so a whole segment taken from a different store and dropped in at a
matching base **is** accepted, and recovery serves the other store's values.
`TestASegmentLiftedFromAnotherStoreAtAMatchingBoundaryIsAccepted` does exactly
that and records the result.

That is a statement about the threat model, not the crash model. No crash
produces that state. crc32c is a checksum and not a MAC: it detects damage, and
it cannot detect substitution by anyone who can write to the store's directory -
inside a segment or across a boundary. The smallest change that would close the
boundary is a per-store identity written at `Open` and used to seed each
segment's chain in place of zero, which does not need the predecessor segment to
still exist. It is not implemented.

`checkpoint.go` has the four-step ordering that makes checkpointing safe, and
the invariant behind it: a segment is only ever removed after the checkpoint that
supersedes it is on disk, so there is no instant at which neither holds the data.

### The directory sync that was removed, and why that is not a shortcut

`writeCheckpoint` used to fsync the directory twice - once between creating
`CHECKPOINT.tmp` and renaming it over `CHECKPOINT`, and once after the rename.
The first was removed. Deleting a durability call because no test fails on it is
one inch from deleting it because it was inconvenient, and the two look the same
in a diff, so the argument is written out here to be argued with.

**The window.** A crash after `createTrunc` and before the fsync that follows
the rename. Crashing at each call in that window, under both versions, leaves
exactly these directories on the platter - measured through the simulated disk,
not reasoned about:

| crash point | one dir fsync | two dir fsyncs |
|---|---|---|
| end of `rename` | `LOG.…0` | `CHECKPOINT.tmp`, `LOG.…0` |
| end of the fsync after it | `CHECKPOINT`, `LOG.…0` | `CHECKPOINT.tmp`, `LOG.…0` |

**Why both are safe.** In this window the checkpoint has not been *installed*:
`writeCheckpoint` has not returned, so `checkpointLocked` has not rotated the log
and has not unlinked one segment. Recovery therefore finds either checkpoint and
the complete log, and replays it. A stray `CHECKPOINT.tmp` is not read by
`loadCheckpoint`, is not a segment name, and is truncated away by the next
`createTrunc`. The one state that would be dangerous - the new name durable over
contents that are not - is prevented by the `f.Sync()` before the rename, not by
any directory sync. And the rename cannot become durable without the create,
because both are writes to the same directory and there would be no entry for
`rename(2)` to move: which is what makes one post-rename fsync sufficient, and is
the canonical write-tmp, fsync-tmp, rename, fsync-dir recipe rather than anything
invented here.

**The scope of that last sentence, which used to be left implicit.** Directory
operations committing in order is a property of ext4 with the default
`data=ordered` and of XFS. POSIX guarantees nothing of the kind, and the
simulated disk here cannot stage a counter-example either, because it promotes
every pending directory entry at once. So this is an argument about the two
filesystems the CI runners use rather than a portable one, and nothing in this
repository tests it. `checkpoint.go` says so at the call site now instead of
leaning on the word "journals" to carry it.

**What cannot be checked.** No test here distinguishes one directory sync from
two. Both pass, and the whole suite passes under both, because the only
difference is a file no reader consults. That is a real gap and it is not closed.
What is checked instead is the premise the argument stands on:
`TestAPowerCutAnywhereInTheCheckpointPathLosesNothing` takes the power out at the
end of every one of the ten filesystem calls the checkpoint path makes, one run
per call, and requires every acknowledged key to be readable afterwards. Invert
the ordering in `checkpointLocked` so segments are unlinked before the checkpoint
is durable and five of those ten fail, naming the keys that went.

## Red proofs

Every case in `verify/kvstore.task.json` was observed failing before it passed,
with three declared exceptions listed at the end of this section, and the record
is committed in
`.general-harness/redproof.json` - the test name that failed, when, and against
which version of the contract. A test that has never been seen to fail has not
been shown to be wired to anything.

The stronger version of that evidence is in the git history rather than in the
JSON, because a file the author's own tooling wrote is weaker proof than a build
a stranger can check out and run. Several commits are deliberately broken and
are labelled `(red)` in their subject: the log's fsync deleted, the directory's
fsync deleted, the torn tail appended past rather than written over, the
negative control disarmed,
the honest write path replaced with the buffering one so the crash corpus can be
watched catching a store that really loses data, and one that is a genuine
failing test rather than a staged break - the commit where a store that could
not sync went on acknowledging writes it could no longer recover. Each is
followed by the commit that answers it. `git checkout <sha> && go test -count=1 ./...`
is the check.

### How the ledger was re-established

`general-harness` keys each proof to the hash of the contract it was recorded
against, so every case added to `verify/kvstore.task.json` stales the proofs that
came before it. Rather than edit the contract to match old hashes - which would
be forging the evidence rather than gathering it - every case was observed
failing again, against the contract as it now stands, in twelve runs of
`general-verify red`. Each run breaks something, runs the whole gate, records
which declared cases were seen failing, and is then reverted.

The contract has gained cases twice now - two when the
checkpoint-on-a-poisoned-segment defect and the stopped-replay case were
declared, and four more for the three untested lines a reviewer found and the
32-bit decoder bug that turned up with them - and each time the hash moved and
staled every proof recorded before it. That is the mechanism working rather than
a defect, and the price of it is this campaign, which is about half an hour of
the gate running against a deliberately broken tree. Runs are ordered broad
first and narrow last on purpose, because the ledger keeps the *last* recording
for each case.

| what was broken | cases whose proof finally came from this run |
|---|---|
| `decodeRecord`: the checksum comparison and the sequence comparison disabled, and every length failure reported as a checksum mismatch | `corrupt-byte-ends-recovery`, `torn-tail-record-discarded`, `sequence-break-ends-recovery`, `absurd-length-field-is-a-torn-tail` |
| `walpolicy.go`: the honest commit replaced with the buffering one from the `kvearlyack` build - acknowledge now, flush every 4KB | `acked-write-survives-immediate-kill`, `unacked-write-may-vanish`, `fsync-precedes-ack` |
| five independent deletions: `readErr = err`, `os.O_EXCL`, the `Truncate` in `reopenSegment`, `sort.Strings` in `encodeState`, and `earlyAckFlushBytes` set to 1 so the negative control is disarmed | `replay-is-byte-identical`, `snapshot-order-independent-of-insert-order`, `seeded-corpus-no-acked-loss`, `seed-reproduces-failure`, `broken-build-fails-the-power-cut-test`, `recovery-read-failure-is-not-the-end-of-the-log`, `create-refuses-a-name-that-exists` |
| `createSegment`'s `syncDir` and its `dir-sync` event; the checkpoint trigger in `write` and `PutBatch`; `loadCheckpoint`'s checksum comparison | thirteen, including `dir-fsync-on-log-create`, `log-bounded-under-sustained-writes`, `partial-checkpoint-is-ignored`, `recovery-after-checkpoint-preserves-acked` and `crash-corpus-recovery-is-deterministic` |
| the `syncDir` after the checkpoint rename | `checkpoint-is-durable-as-soon-as-it-is-installed`, `power-cut-anywhere-in-the-checkpoint-path-loses-nothing` |
| the crash harness: a fixed kill offset in place of the randomised one, the corpus generator's origin constant, and the `Skipped > 0` branch of `Shape` | `kill-points-are-spread`, `corpus-size-floor`, `every-recovery-shape-is-tallied` |
| the `r.seq <= CheckpointSeq` skip in `loadDir`, and nothing else | `stopped-replay-never-re-applies-superseded-records` |
| the `s.log.failed` guard at the top of `checkpointLocked`, and nothing else | `checkpoint-never-rotates-away-from-a-failed-segment` |
| the abut check between segments in `loadDir`, and nothing else | `gap-between-segments-stops-recovery` |
| the refusal to open when the oldest segment starts past the checkpoint, and nothing else | `log-starting-past-the-checkpoint-is-refused` |
| the `maxPayload` ceiling in `decodeRecord`, and nothing else | `length-past-the-ceiling-stops-before-the-checksum` |
| the `sort.Strings` that fixes the order of the findings a replay reports, and nothing else | `replayed-findings-are-in-a-stable-order` |

The last row was a twelfth run and it is here because the first attempt at that
break was aimed at the wrong `sort.Strings`. The crash-harness run above broke
the one in `Kinds()`, which sorts the *kinds* a result reports and not the
findings themselves, and `TestReplayedFindingsAreInAStableOrder` stayed green
under it - so that case came out of the campaign still carrying a proof against
the previous contract hash. The one that matters is the sort of `wantKeys` in
`replayCase`, which is what puts the findings in a fixed order rather than Go's
randomised map order. Breaking that one turns the test red. Worth recording
rather than quietly re-running: a break that misses is exactly how a case ends
up looking proven when it is not.

A run records every case whose test failed, so where a break was broad some
cases were recorded by more than one of these and the ledger keeps the last.
The table says which run each proof finally came from, and the honest reading of
a broad break is that it shows the test is wired to *something* the break
touched; a narrow one shows more. The last six rows are narrow on purpose and touch
exactly one line each.

Two of the proofs are worth separating from the rest, because they are the only
ones in this ledger recorded in the order TDD actually prescribes rather than by
sabotaging finished code. `checkpoint-never-rotates-away-from-a-failed-segment`
was recorded against a tree where the fix did not exist yet - the test was
written, the case declared, the gate run, and the guard added afterwards. It was
then re-recorded narrowly at the end, because the broad run above it had
overwritten the entry. `stopped-replay-never-re-applies-superseded-records` is
the other way round: the code it protects was already there, so the only honest
proof available is to remove it and watch, which is what the last-but-one row
is.

The rest were recorded by sabotage, and that is the weaker kind of evidence.
It shows a test is wired to a line; it does not show the test came first.

### Individual deletions

These lines have each been deleted, with the suite run against the result:

| line removed | what turns red |
|---|---|
| `w.f.Sync()` in `walpolicy.go` commit() | `TestAckedWriteSurvivesASimulatedPowerCut` |
| `fsys.syncDir()` in `createSegment` | `TestANewSegmentsDirectoryEntryIsMadeDurable`, and most of the suite |
| `fsys.syncDir()` after the checkpoint rename | `TestTheCheckpointIsDurableAsSoonAsItIsInstalled` |
| `s.fsys.syncDir()` after unlinking superseded segments | `TestACheckpointStillBoundsTheLogAfterAPowerCut` |
| the write offset in `reopenSegment` (`wrote: validBytes` becomes `wrote: size`) | `TestATornPageLeavesEveryAcknowledgedWriteRecoverable` |
| `f.Truncate(validBytes)` in `reopenSegment`, on its own | `TestAStoreWhoseSegmentFailedReopensAndResumes`, and nothing else - see the correction below |
| `f.Sync()` in `writeCheckpoint` | `TestCheckpointedStateSurvivesASimulatedPowerCut` |
| `readErr = err` in `readAll` (`file.go`) | `TestARecoveryReadFailureIsReportedRatherThanTakenAsTheEndOfTheLog` |
| `os.O_EXCL` in `osFS.create` | `TestCreatingAFileThatAlreadyExistsFails` |
| the crc reseed in `record.go` | `TestRecordFromAnotherChainIsRejected` |
| the `r.seq <= CheckpointSeq` skip in `loadDir` | `TestAStoppedReplayNeverReAppliesSupersededRecords`, on all ten keys and on the snapshot |
| the `s.log.failed` guard at the top of `checkpointLocked` | `TestACheckpointNeverRotatesAwayFromAFailedSegment`, on two acknowledged keys missing after a clean reopen |
| the abut check between segments in `loadDir` | `TestRecoveryRefusesToServeRecordsPastAMissingSegment`, on ten keys and the snapshot |
| the refusal to open when the log starts past the checkpoint | `TestRecoveryRefusesToOpenWhenTheLogStartsPastTheCheckpoint`, on ten keys |
| the `maxPayload` ceiling in `decodeRecord` | `TestALengthPastTheCeilingStopsBeforeTheChecksum`, and nothing else in the suite |
| one key dropped from the payload `writeCheckpoint` is given | nine tests, each naming the key that went: `recovered 319 keys, want 320`, and an acknowledged key absent at six of the ten crash points in the checkpoint path. A tenth failure is the line-count test above, noticing that the sabotage added six lines of Go |

The three deletion rows before the last are a reviewer's finding from the turn
before, closed. All three lines were deleted with `go test ./...` green,
240-seed corpus included, and the first of them is the premise this page argues
from when it downgrades the superseded-record skip. What that cost is the last
section here.

One row in that table used to be wrong and is corrected above. It read
`f.Truncate(validBytes)` against the torn-page test, from the deliberately
broken commit `15d3c1b`. That commit deletes two things, not one: the truncation
*and* the write offset, which it moves from `validBytes` to `size`. The offset is
what the torn-page test catches - appends here are positional, so with
`wrote: validBytes` the next record is written at the tear and over it whatever
the file's length is. Deleting the `Truncate` alone left the entire suite green,
root package and 240-seed corpus both, until
`TestAStoreWhoseSegmentFailedReopensAndResumes` began checking the segment's
length against what recovery vouched for. The call stays: it is what keeps a
segment from carrying an abandoned record's tail around for the rest of its life,
which is a narrower claim than the one the code comment used to make.

And two that were deleted and did **not** turn anything red, which is why they
are here:

| line removed | what happened |
|---|---|
| `f.Sync()` after the truncation in `reopenSegment` | nothing. The next append's fsync promotes the staged length anyway, so the only window is a store that recovers from a tear and then crashes without writing - and that crash loses nothing, because recovery truncates again. |
| `fsys.syncDir()` after removing unreachable segments (`store.go`) | nothing. Those segments hold no record recovery can reach *in the case that produces them* - a torn tail in the newest segment, which is what a power cut leaves. The qualifier was missing when this row was written and it matters: a segment beyond a *gap* is perfectly readable, and recovery declines to reach it by choice. See the last section. |

Four contract cases are declared guards rather than probes, which means they
carry no red proof and are never counted as proven:

- the out-of-order flush test, because this store never had a scanning recovery
  to break;
- the failed-write test, which is green on any implementation the failed-sync
  and short-write tests are also green on - a write that took no bytes leaves no
  gap to write past;
- the lifted-segment test, which records a limitation rather than a guarantee,
  so there is no implementation for it to catch;
- the unlink-order test, which is the same kind and is new this turn. It
  measures what a segment set left in any order but oldest-first costs, and what
  it costs is ten acknowledged records. It pins today's behaviour so that no
  argument can lean on the unlink order again without saying what the order is
  worth.

Manufacturing a red for any of them would be manufacturing evidence.

### Every assertion on a report field, audited - sixteen of them

A reviewer deleted the `if r.seq <= st.report.CheckpointSeq` guard in `loadDir`.
One test caught it - and caught it on `rep.Skipped == 0`, a counter the store
fills in about itself. Downgrading that single `Fatalf` to a `Logf` left the
sabotaged build passing the same test's data loop *and* its byte-for-byte
snapshot comparison. Which is the fsync story again, two files along: the whole
reason `v0.1.1` exists is that the evidence for the fsync was an event the store
emitted and a counter the store incremented, and here was the same shape sitting
untouched.

The fair question is why the insight did not generalise the first time. It did
not because the fix was aimed at *the fsync* rather than at *the shape*, and
nothing went looking for the shape anywhere else. So this is that sweep: every
assertion in the suite that reads a field the store or the harness reports about
itself, and for each one, whether anything fails **on data** if the field is
wrong.

Three roles, and only the third is a problem.

- **Precondition** - the assertion is about the *test's own setup*, not about the
  store. `s.Stats().Checkpoints == 0` means "this test never reached the path it
  is named for". Failing it means the test is inert, which is worth a hard stop,
  and it makes no claim about correctness.
- **Property with a data backstop** - the field is asserted, and the same defect
  also breaks something a `Get` can see.
- **Sole detector** - the field is the only thing that fires. Two of these
  existed and both were changed this turn.

| assertion | file | role | if the field lies, does data fail? |
|---|---|---|---|
| `rep.Skipped == 0` | `checkpoint_test.go` | precondition (relabelled this turn) | no, and it cannot - see below |
| `Recovery().Segments > 2 && peak == 0` | `checkpoint_test.go` | precondition | n/a; the test's bound assertions are on directory bytes |
| `!rep.UsedCheckpoint` | `checkpoint_test.go` | precondition | yes, the per-key loop below it |
| `!rep.CheckpointRejected`, `rep.UsedCheckpoint` | `checkpoint_test.go` | property | **yes, measured** - disabling the checksum comparison in `loadCheckpoint` fails 22 data assertions in the same subtest alongside these two |
| `r.Stopped != stopTornRecord` | `durability_test.go` | property | yes; `a`/`b`/`c` present and `d` absent are asserted first |
| `Stats().Syncs - before != 1` (x2) | `durability_test.go` | **narration** | **no.** The trace assertion beside it is narration too. This is the pair the README's headline story is about; the backstop is the simulated disk, and as of this turn `Stats().Syncs` is checked against the disk's own count in `TestAckedWriteSurvivesASimulatedPowerCut` |
| `Stats().Records != writers*perWriter` | `durability_test.go` | narration | no. The property it stands in for - no acknowledged write lost under concurrency - is asserted on data by `reopened.Len()` and the per-key loop immediately below, which are stronger |
| `disk.Syncs() == 0` | `powercut_test.go` | precondition | the *disk's* counter, not the store's: a count taken outside the subject |
| `disk.Syncs() == Stats().Syncs + 1` | `powercut_test.go` | **new this turn** | this is the line that makes the two `durability_test.go` assertions accountable |
| `r.Stopped` (x3) | `powercut_test.go` | property | yes; each is preceded by per-key assertions, and the out-of-order case also compares two independent recoveries byte for byte |
| `Stats().Checkpoints` (x4) | `powercut_test.go`, `segmentboundary_test.go` | precondition | n/a |
| `r.CheckpointSeq == 0 && r.UsedCheckpoint` | `powercut_test.go` | precondition | no; the per-key data loop three lines below it is what decides that subtest |
| `!r.UsedCheckpoint` | `powercut_test.go` | was a **sole detector** | no, and nothing is lost in that window either way. Fixed this turn: the test now asks `disk.DurableNames()` what is on the platter before it asks the store, and that line fires on its own |
| `r.Stopped != stopEndOfLog` | `segmentboundary_test.go` | records a limitation | the assertion below it names whose data came back |
| `Kinds()`, `len(Failures)` | `crashtest/crash_test.go` | property | these are not store fields. A `Failure` is derived from a `Get`: `acked-write-lost`, `corrupt-read`, `phantom-read`, `nondeterministic-recovery`. The corpus's verdict is data |
| `Shape()` reads four report fields | `crashtest/shape.go` | **descriptive** | nothing passes or fails on a shape. The tally says what the corpus reached; the verdict is decided by the findings above it |

Two of those rows are worth reading properly.

**`rep.Skipped` cannot be made to fail on data, and that is the answer.** The
window the test stages - a crash after the checkpoint is durable and before the
old segments are unlinked - is idempotent. The checkpoint is a fold of the
complete prefix, so replaying a *suffix* of that prefix over it lands back on
the same values, and a suffix is exactly what survives, because
`checkpointLocked` unlinks oldest first and `loadDir` refuses a gap between
segments. Under this store's own ordering the guard buys work, not correctness.
The comment claiming it prevents "the exact bug that makes a crash during
checkpointing corrupt a store" was more than the code does, and it has been
downgraded to what is true.

Where the guard *is* load-bearing is one step further out: if replay **stops**
partway through the superseded range, what has been applied is a *prefix*, and a
prefix puts back values the checkpoint already superseded.
`TestAStoppedReplayNeverReAppliesSupersededRecords` builds that directory - a
checkpoint at sequence 30 over a segment holding records 1..15 and then eight
bytes of a record that never finished - and without the guard all ten keys come
back stale, `k00`..`k04` to round 1 and `k05`..`k09` to round 0, plus the
snapshot comparison. That directory is hand-built, and the test says so: it
needs a torn tail in a segment that is not the last one, which the store does
not produce. It is a log the *format* admits, and `loadDir` should not depend on
an invariant maintained in `checkpoint.go` for the price of one comparison per
record.

**A second consequence of the same directory, recorded rather than fixed.** When
replay stops inside the superseded range, the recovered sequence counter is the
last record it read - 15 in that test - while the checkpoint claims 30. The next
record written would take a number the checkpoint already covers, and the
recovery after that would skip it. The test therefore stops at the reopen and
does not write. It is not fixed here because the obvious repair - advance the
counter to the checkpoint's sequence - breaks the within-segment chain, which
starts at `base+1` and expects consecutive numbers; making it work means moving
the sequence chain into the segment header, and that is a log format change with
the 240-seed corpus and CI downstream of it. It is reachable only from the same
directory the store does not produce.

**`Stats().Syncs` was under-documented rather than wrong.** Checking it against
the disk's own count turned up a difference: three acknowledged `Put`s, three
commit fsyncs counted by the store, four fsyncs seen by the disk. The fourth is
`createSegment` fsyncing the empty segment file before a record is in it, which
is not a commit. The assertion pins that relationship rather than asserting
equality - making the counter's definition follow the disk would have been the
wrong repair - and `Stats.Syncs` now documents which fsyncs it counts.

**The table was fifteen rows when it was written and the heading said
"every".** The sixteenth - `r.CheckpointSeq == 0 && r.UsedCheckpoint`, the first
row above - came from a reviewer's independent grep rather than from this
audit's. It is the least dangerous row on the page: a precondition with the
per-key loop directly beneath it, so the defect was the completeness word and
not the assertion. It is worth leaving in view, because a section about an
assertion standing in for evidence that itself over-claims by one row is the
same failure one level up. The count is in the heading now, so the claim rots
into a number a reader can check rather than into a word nobody can.

Three tests were added after this audit - the two gap tests and the unlink-order
one - and none of them asserts on a report field. That was deliberate: the
directories they build are hand-made, so a report field would have been the easy
thing to assert and the wrong thing.

What this audit does **not** establish: that no other assertion anywhere is
weaker than it looks. It covers assertions on *report and counter fields*, found
by grepping for `rep.`, `Stats()`, `Recovery()`, `Skipped`, `Applied`,
`Dropped`, `Stopped`, `Syncs` and `Checkpoints`. An assertion that is
tautological for some other reason would not show up in it - and the section
below is what happened when someone went looking for one that was.

### Every premise these arguments stand on, swept

A reviewer deleted three lines this page never nominated - the abut check
between segments in `loadDir`, the refusal to open when the oldest segment
starts past the checkpoint, and the `maxPayload` ceiling in `decodeRecord` -
and `go test ./...` stayed green on all three, 240-seed corpus included.

The first one is not just any untested line. The section above uses it as the
load-bearing premise of this document's own retraction: the superseded-record
skip was downgraded from a correctness guarantee to "buys work, not
correctness" *because* what a crash mid-deletion leaves is a suffix, and it is
a suffix *because* `loadDir` refuses a gap. The sentence that withdrew one
claim was resting on a line nothing in the suite touched.

**That is the same shape a third time and the generalisation had been missed
twice.** The first fix was aimed at the fsync rather than at the shape. The
second sweep was aimed at every assertion that reads a counter the store
reports about itself - and that was still a sweep over *assertions*. An
argument is a claim too. Its premises are load-bearing lines exactly as much as
the code they justify, and they are worse than untested code, because a premise
is what makes deleting the line it names look safe.

So: every place this repository says *X is safe because Y*, in this document
and in the comments of the ten shipping files, with Y named and broken.

| the argument | its premise `Y` | what fails when `Y` is broken |
|---|---|---|
| re-applying superseded records is a no-op on data | `loadDir` refuses a gap between segments | **nothing, until this turn.** Now `TestRecoveryRefusesToServeRecordsPastAMissingSegment`: ten keys and the snapshot |
| the same argument | `checkpointLocked` unlinks oldest first, so what survives is a suffix | nothing exercises the order itself. What the alternative costs is now measured instead - `TestAnUnlinkOrderOtherThanOldestFirstLosesAcknowledgedWrites`, ten acknowledged records gone with no crash anywhere. The premise is load-bearing, and for a reason the argument never gave: see below |
| the same argument | the checkpoint is a fold of the *complete* prefix | **measured.** Drop one key from the payload `writeCheckpoint` is handed and nine tests fail on data, each naming the key that went - `recovered 319 keys, want 320`, and an acknowledged key absent at six of the ten crash points in the checkpoint path. The tests existed already; the measurement did not |
| an unreadable checkpoint is treated as absent, because the log is authoritative | segments are only removed after the checkpoint that supersedes them is durable | if that ever failed, the log would start past sequence 0 with no checkpoint, and the refusal at the top of `loadDir` is what catches it: `TestRecoveryRefusesToOpenWhenTheLogStartsPastTheCheckpoint`. **Nothing caught it before this turn** |
| the four steps of `checkpointLocked` leave no window in which neither the checkpoint nor the log holds the data | the order of the four | `TestAPowerCutAnywhereInTheCheckpointPathLosesNothing`. Invert it and five of ten crash points fail, naming the keys. Proven before this turn |
| one post-rename directory fsync is sufficient | the rename cannot become durable before the create | **nothing here can break it.** The simulated disk promotes every pending directory entry at once, so the falsifying state cannot be staged. It is an ext4/XFS journal-ordering property rather than a POSIX one, and both the comment and the section above now say so instead of implying it |
| the segments `OpenWith` deletes hold no record recovery can reach | that the stop point is a torn tail in the newest segment | true of every directory the store produces and **false in general**. A segment beyond a gap verifies perfectly; recovery declines to reach it by choice, and then the deletion makes the choice permanent. Measured by the unlink-order test above and now written at the deletion rather than only here |
| the `maxPayload` ceiling stops a 3.9GB `make([]byte)` | that there is an allocation on that path | **there is none.** `decodeRecord` slices a buffer `readAll` already produced, and `len(buf) < total` rejects the same input a line later, which is why deleting the ceiling left everything green. The two real reasons are in `record.go` now, and each has a test |
| a record lifted from elsewhere fails, because the checksum is chained | the chain restarts at zero at each segment boundary | already a declared limitation with its own test: a whole segment lifted at a matching base is accepted. Proven as a limitation rather than as a guarantee |
| `replayBytes` never skips a record | it returns at the first record it cannot vouch for | the three framing tests. Proven |
| a torn tail cannot swallow later writes | appends are positional - `wrote: validBytes`, not `size` | `TestATornPageLeavesEveryAcknowledgedWriteRecoverable`. Proven, and the row above it in the deletions table is the correction of an earlier version of this same claim |
| the crash corpus cannot catch a missing fsync | a process kill leaves the page cache intact | measured rather than argued: the deliberately buffered build is caught on 13 of 24 seeds on Windows and 4 of 24 on Linux, and a build with no fsync at all is caught on none |

**The one that changed my mind.** The unlink order was written about as tidiness
- oldest first, so what survives is a suffix - and used twice as a premise
without anyone asking what the other order costs. It costs ten acknowledged
records, and the mechanism is not the abut check failing to fire. The abut check
fires exactly as designed: it stops replay at the hole. What loses the data is
the next step, `OpenWith` unlinking every segment past the stop point. So the
premise is load-bearing because of a *different* line than the argument names,
and no amount of re-reading the argument would have found that. Breaking it did.

**What this does to the `v0.1.1` retraction.** Less than it might look, and not
nothing. `v0.1.1` retracted the evidence for B2 - a trace-ordering assertion and
a counter the store incremented - and that retraction stands untouched; the
simulated platter replaced it and can be checked. What is weaker is the
*accompanying* downgrade of the superseded-record skip. Its conclusion survives:
replaying a suffix of a folded prefix really is a no-op, and the reviewer's own
sabotage of that window confirmed it. But at the time it was written it was an
argument resting on two lines nothing exercised, one of which turns out to be
load-bearing for a reason the argument did not contain. A retraction issued on
the strength of an unexamined premise is a smaller thing than it reads as, and
the honest summary is that the conclusion was right and the reasoning had not
earned it yet.

**A test nobody had run.** The third deleted line, the `maxPayload` ceiling,
turned up something the sweep was not looking for.
`TestAbsurdLengthFieldIsATornTail` had been failing since the day it was
written, on any 32-bit build, because nobody had made one:

```sh
GOARCH=386 go test -count=1 .        # at 1c81fe6, and at every commit before it
```

`int` is 32 bits there and the ceiling converted the on-disk `uint32` before
comparing it, so `0xFFFFFFF0` became `-16` and passed under a 16MiB ceiling. A
length with the high bit set is worse: `headerSize + payload` overflows negative
and the slice that follows panics inside recovery, on precisely the input
recovery exists to survive.

```
panic: runtime error: slice bounds out of range [:-2147483627]
kvstore.decodeRecord ... record.go:138
kvstore.replayBytes  ... recover.go:29
```

The fix is to compare the `uint32` before converting it. `29145d4` is the test
committed red - green on amd64, red on 386 - and `4a13a3e` is the fix. CI now
runs the root package under `GOARCH=386` on the Windows job, where a 386 binary
has been observed to execute; the Linux runner would need ia32 support that has
not been checked here, and a CI step whose green has never been seen is the
thing this repository argues against.

**What the sweep does not establish.** It is a sweep over arguments found by
reading, not by a tool: `because`, `so that`, `which is why` and `the reason`
across the shipping files and this page, then keeping the ones that justify a
safety property. An argument nobody wrote down cannot appear in it, and neither
can a premise stated so vaguely that breaking it has no definite meaning. The
honest position is the same one the section above ends on: this is a longer list
than it was, and it is still not the complete list.
