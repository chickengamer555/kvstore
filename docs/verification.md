# Verification

The long form of the evidence behind [the README](../README.md): what the two
harnesses were measured to reach, which platform settles which claim, how to
reproduce a failure, where the benchmark numbers came from, and which lines in
this repository have been observed to be load-bearing by deleting them.

Nothing here is a stronger claim than the README makes. It is the same claims
with the measurements attached.

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

## Red proofs

Every case in `verify/kvstore.task.json` was observed failing before it passed,
with one declared exception, and the record is committed in
`.general-harness/redproof.json` - the test name that failed, when, and against
which version of the contract. A test that has never been seen to fail has not
been shown to be wired to anything.

The stronger version of that evidence is in the git history rather than in the
JSON, because a file the author's own tooling wrote is weaker proof than a build
a stranger can check out and run. Several commits are deliberately broken and
are labelled `(red)` in their subject: the log's fsync deleted, the directory's
fsync deleted, the torn-tail truncation deleted, the negative control disarmed,
the honest write path replaced with the buffering one so the crash corpus can be
watched catching a store that really loses data, and one that is a genuine
failing test rather than a staged break - the commit where a store that could
not sync went on acknowledging writes it could no longer recover. Each is
followed by the commit that answers it. `git checkout <sha> && go test -count=1 ./...`
is the check.

These lines have each been deleted, with the suite run against the result:

| line removed | what turns red |
|---|---|
| `w.f.Sync()` in `walpolicy.go` commit() | `TestAckedWriteSurvivesASimulatedPowerCut` |
| `fsys.syncDir()` in `createSegment` | `TestANewSegmentsDirectoryEntryIsMadeDurable`, and most of the suite |
| `fsys.syncDir()` after the checkpoint rename | `TestTheCheckpointIsDurableAsSoonAsItIsInstalled` |
| `s.fsys.syncDir()` after unlinking superseded segments | `TestACheckpointStillBoundsTheLogAfterAPowerCut` |
| `f.Truncate(validBytes)` in `reopenSegment` | `TestATornPageLeavesEveryAcknowledgedWriteRecoverable` |
| `f.Sync()` in `writeCheckpoint` | `TestCheckpointedStateSurvivesASimulatedPowerCut` |
| `readErr = err` in `readAll` (`file.go`) | `TestARecoveryReadFailureIsReportedRatherThanTakenAsTheEndOfTheLog` |
| `os.O_EXCL` in `osFS.create` | `TestCreatingAFileThatAlreadyExistsFails` |
| the crc reseed in `record.go` | `TestRecordFromAnotherChainIsRejected` |

And two that were deleted and did **not** turn anything red, which is why they
are here:

| line removed | what happened |
|---|---|
| `f.Sync()` after the truncation in `reopenSegment` | nothing. The next append's fsync promotes the staged length anyway, so the only window is a store that recovers from a tear and then crashes without writing - and that crash loses nothing, because recovery truncates again. |
| `fsys.syncDir()` after removing unreachable segments (`store.go`) | nothing. Those segments hold no record recovery can reach. |

The one contract case declared a guard rather than a probe is the out-of-order
flush test, which has no honest red proof because this store never had a
scanning recovery to break. Manufacturing one to have something to catch would
be manufacturing evidence.
