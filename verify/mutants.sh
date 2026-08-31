#!/usr/bin/env bash
#
# A mutation sweep over the safety decisions in this store.
#
# WHY THIS EXISTS. The premise sweep in docs/verification.md finds untested
# lines by READING: it collects every "X is safe because Y" in the shipping
# files and breaks each Y. It found three that nothing exercised. It cannot
# find a fourth kind - a line whose argument nobody wrote down, or whose
# argument is true and untested anyway - and a reviewer then found exactly
# that by reversing one loop and watching the whole suite stay green. This is
# the same procedure, mechanical, so it does not depend on a reviewer showing
# up.
#
# Each mutation below is a single edit that a competent person could plausibly
# make: a loop direction, a comparison, a step order, a missing call. If the
# suite stays green under one, that mutation is a gap, and it is a gap found
# without anyone having to have an opinion first.
#
# WHAT IT IS NOT. Eight hand-written mutations are not mutation coverage. A
# real mutation tester enumerates every mutable token; this enumerates the ones
# someone thought of, which is the same limitation the reading sweep has, one
# step further out. It is cheaper than the reading sweep and catches a
# different shape, and both of them together are still not "the complete list".
#
# The README line-count assertion is excluded, because several of these
# mutations change the number of lines in the tree and it would fire on the
# arithmetic rather than on the behaviour. Nothing else is excluded.
#
# It runs the ROOT package only. The 240-seed crash corpus is 150 seconds a
# run, which would make this twenty minutes and nobody would run it. That is a
# real limitation and it cuts the wrong way, because the corpus is the harness
# with the widest reach. Where a mutant survives here, re-run it against
# `go test -count=1 ./crashtest/` before believing the survival.
#
# Usage:  bash verify/mutants.sh          # all of them, about 90 seconds
#         bash verify/mutants.sh 3        # just number 3
#
# Requires a clean tree. It edits files in place and restores them with git
# checkout after each run, so a dirty tree would lose work.

set -u
cd "$(dirname "$0")/.."

if [ -n "$(git status --porcelain)" ]; then
  echo "verify/mutants.sh: the tree is dirty, and this script restores files with git checkout." >&2
  echo "Commit or stash first." >&2
  exit 2
fi

# id | file | what it breaks | sed expression
mutants=(
'1|checkpoint.go|unlink newest first instead of oldest first|s@^\tfor _, base := range bases {$@\tfor i := len(bases) - 1; i >= 0; i-- {\n\t\tbase := bases[i]@'
'2|checkpoint.go|do not sync the directory after the unlinks|s@^\tif err := s.fsys.syncDir(); err != nil {$@\tif err := error(nil); err != nil {@'
'3|recover.go|open anyway when the log starts past the checkpoint|s@^\tif len(segs) > 0 \&\& segs\[0\] > st.report.CheckpointSeq {$@\tif false {@'
'4|recover.go|do not require segments to abut|s@^\t\tif i > 0 \&\& base != seq {$@\t\tif false {@'
'5|recover.go|re-apply superseded records instead of skipping them|s@^\t\t\tif r.seq <= st.report.CheckpointSeq {$@\t\t\tif false {@'
'6|recover.go|keep the segments past the point replay stopped|s@^\t\tst.drop = append(st.drop, segs\[stopAt+1:\]...)$@\t\t_ = stopAt@'
'7|record.go|accept a length field the record cannot contain|s@^\tif total > maxPayload {$@\tif false {@'
'8|wal.go|acknowledge before the fsync rather than after it|@'
)

run_one() {
  local spec="$1"
  local id file what expr
  id="${spec%%|*}"; spec="${spec#*|}"
  file="${spec%%|*}"; spec="${spec#*|}"
  what="${spec%%|*}"; expr="${spec#*|}"

  if [ -z "$expr" ]; then
    printf '%-3s %-14s %-52s SKIP  (has its own red proof; see docs/verification.md)\n' "$id" "$file" "$what"
    return
  fi

  local before after
  before=$(git hash-object "$file")
  sed -i -e "$expr" "$file"
  after=$(git hash-object "$file")
  if [ "$before" = "$after" ]; then
    git checkout -- "$file"
    printf '%-3s %-14s %-52s ERROR (the mutation did not apply - the line moved)\n' "$id" "$file" "$what"
    return
  fi

  local out failures
  out=$(go test -count=1 . 2>&1)
  git checkout -- "$file"

  # The line-count assertion is dropped here rather than by -run, because -run
  # would also drop whatever else happened to match the pattern. Every other
  # failure counts.
  failures=$(printf '%s\n' "$out" | grep -o -- '--- FAIL: [A-Za-z]*' | sed 's/--- FAIL: //' \
    | grep -v '^TestTheLineCountInTheReadmeIsTheLineCountOfTheTree$' | sort -u)

  if [ -n "$failures" ]; then
    printf '%-3s %-14s %-52s CAUGHT by %s\n' "$id" "$file" "$what" \
      "$(printf '%s\n' "$failures" | head -3 | paste -sd, -)"
  else
    printf '%-3s %-14s %-52s SURVIVED - nothing in the root suite sees this\n' "$id" "$file" "$what"
  fi
}

if [ $# -gt 0 ]; then
  for spec in "${mutants[@]}"; do
    [ "${spec%%|*}" = "$1" ] && run_one "$spec"
  done
else
  for spec in "${mutants[@]}"; do run_one "$spec"; done
fi
