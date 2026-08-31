package kvstore

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// buildLog encodes n records with keys k1..kn and values v1..vn, chained from
// the given starting sequence number. Returns the bytes and the offset at which
// each record starts, so a test can damage a specific one.
func buildLog(t *testing.T, seqs []uint64) ([]byte, []int) {
	t.Helper()
	var buf []byte
	var crc uint32
	offsets := make([]int, 0, len(seqs))
	for i, seq := range seqs {
		offsets = append(offsets, len(buf))
		buf, crc = appendRecord(buf, record{
			seq:   seq,
			kind:  kindPut,
			key:   keyN(i + 1),
			value: valueN(i + 1),
		}, crc)
	}
	return buf, offsets
}

func keyN(n int) string   { return string(rune('a'+n-1)) + "-key" }
func valueN(n int) []byte { return bytes.Repeat([]byte{byte('0' + n)}, 8) }
func appliedKeys(rs []record) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.key)
	}
	return out
}

func collect(buf []byte) ([]record, stopReason, int) {
	var got []record
	_, _, consumed, reason := replayBytes(buf, 0, 1, func(r record) { got = append(got, r) })
	return got, reason, consumed
}

func TestRecordRoundTripsThroughReplay(t *testing.T) {
	buf, _ := buildLog(t, []uint64{1, 2, 3})
	got, reason, consumed := collect(buf)

	if reason != stopEndOfLog {
		t.Fatalf("clean log stopped for reason %q, want %q", reason, stopEndOfLog)
	}
	if consumed != len(buf) {
		t.Errorf("consumed %d of %d bytes", consumed, len(buf))
	}
	if want := []string{keyN(1), keyN(2), keyN(3)}; !equalStrings(appliedKeys(got), want) {
		t.Fatalf("applied %v, want %v", appliedKeys(got), want)
	}
	for i, r := range got {
		if !bytes.Equal(r.value, valueN(i+1)) {
			t.Errorf("record %d value = %q, want %q", i, r.value, valueN(i+1))
		}
	}
}

// An empty value and an empty key are the two shapes most likely to be handled
// by accident rather than on purpose.
func TestRecordRoundTripsEmptyKeyAndValue(t *testing.T) {
	var buf []byte
	var crc uint32
	buf, crc = appendRecord(buf, record{seq: 1, kind: kindPut, key: "", value: []byte("v")}, crc)
	buf, _ = appendRecord(buf, record{seq: 2, kind: kindPut, key: "k", value: nil}, crc)

	got, reason, _ := collect(buf)
	if reason != stopEndOfLog {
		t.Fatalf("stopped for reason %q, want %q", reason, stopEndOfLog)
	}
	if len(got) != 2 {
		t.Fatalf("applied %d records, want 2", len(got))
	}
	if got[0].key != "" || !bytes.Equal(got[0].value, []byte("v")) {
		t.Errorf("record 0 = %q/%q, want empty key and value \"v\"", got[0].key, got[0].value)
	}
	if got[1].key != "k" || len(got[1].value) != 0 {
		t.Errorf("record 1 = %q/%q, want key \"k\" and empty value", got[1].key, got[1].value)
	}
}

// B3. A byte flipped inside a record must be caught by the checksum, and
// recovery must stop there - not skip the damaged record and carry on, which
// would silently resurrect the writes that came after it.
func TestCorruptByteEndsRecovery(t *testing.T) {
	buf, offsets := buildLog(t, []uint64{1, 2, 3})

	// Flip a bit in the middle of record 2's payload. The checksum is over
	// everything after the crc field, so this must be detected.
	victim := offsets[1] + headerSize + 2
	buf[victim] ^= 0x40

	got, reason, consumed := collect(buf)

	if reason != stopChecksum {
		t.Fatalf("stop reason %q, want %q - a flipped byte must be caught by the checksum", reason, stopChecksum)
	}
	if want := []string{keyN(1)}; !equalStrings(appliedKeys(got), want) {
		t.Fatalf("applied %v, want %v - recovery must stop at the last intact record", appliedKeys(got), want)
	}
	if consumed != offsets[1] {
		t.Errorf("consumed %d bytes, want %d (up to the start of the damaged record)", consumed, offsets[1])
	}
	for _, r := range got {
		if r.key == keyN(3) {
			t.Fatal("record 3 was applied - recovery jumped over the damaged record")
		}
	}
}

// B3. A crash mid-write leaves a record with its tail missing. That is the
// normal, expected shape of a log after a kill, so it must end recovery
// cleanly rather than being reported as corruption.
func TestTornTailRecordDiscarded(t *testing.T) {
	buf, offsets := buildLog(t, []uint64{1, 2, 3})

	t.Run("truncated mid-payload", func(t *testing.T) {
		torn := append([]byte(nil), buf[:offsets[2]+headerSize+3]...)
		got, reason, consumed := collect(torn)
		if reason != stopTornRecord {
			t.Fatalf("stop reason %q, want %q", reason, stopTornRecord)
		}
		if want := []string{keyN(1), keyN(2)}; !equalStrings(appliedKeys(got), want) {
			t.Fatalf("applied %v, want %v", appliedKeys(got), want)
		}
		if consumed != offsets[2] {
			t.Errorf("consumed %d bytes, want %d", consumed, offsets[2])
		}
	})

	t.Run("truncated mid-header", func(t *testing.T) {
		torn := append([]byte(nil), buf[:offsets[2]+5]...)
		got, reason, _ := collect(torn)
		if reason != stopTornRecord {
			t.Fatalf("stop reason %q, want %q", reason, stopTornRecord)
		}
		if want := []string{keyN(1), keyN(2)}; !equalStrings(appliedKeys(got), want) {
			t.Fatalf("applied %v, want %v", appliedKeys(got), want)
		}
	})

	t.Run("a single trailing zero byte", func(t *testing.T) {
		torn := append(append([]byte(nil), buf...), 0)
		got, reason, _ := collect(torn)
		if reason != stopTornRecord {
			t.Fatalf("stop reason %q, want %q", reason, stopTornRecord)
		}
		if len(got) != 3 {
			t.Fatalf("applied %d records, want all 3 before the stray byte", len(got))
		}
	})
}

// B3. The sequence chain has to be checked independently of the checksum,
// because they catch different things. This log is internally consistent -
// every checksum is correct, every record chains to its predecessor - and its
// sequence numbers go 1, 2, 4. That is the shape of a log that lost a record
// from its middle, and recovery must stop rather than close the gap.
func TestSequenceBreakEndsRecovery(t *testing.T) {
	buf, offsets := buildLog(t, []uint64{1, 2, 4})

	got, reason, consumed := collect(buf)

	if reason != stopSequence {
		t.Fatalf("stop reason %q, want %q - a gap in the chain must not be skipped", reason, stopSequence)
	}
	if want := []string{keyN(1), keyN(2)}; !equalStrings(appliedKeys(got), want) {
		t.Fatalf("applied %v, want %v", appliedKeys(got), want)
	}
	if consumed != offsets[2] {
		t.Errorf("consumed %d bytes, want %d", consumed, offsets[2])
	}
}

// B3. The length prefix is attacker-shaped data after a crash: it is read
// before the checksum can be verified, because the checksum covers it. A
// garbage length must be treated as the end of the log, never as a request to
// allocate that many bytes and never as a slice bound.
//
// What this test does and does not establish, because the comment on
// maxPayload claimed more than the code does. On a 64-bit build every one of
// these lengths is rejected with the ceiling deleted as well as with it: the
// `len(buf) < total` comparison one line further down rejects the same input,
// and decodeRecord slices a buffer that has already been read rather than
// allocating one, so there is no 3.9GB make([]byte) on this path to prevent.
// The ceiling earns its place in two narrower ways and each has its own
// evidence:
//
//   - it keeps the checksum from being computed over a length the record
//     cannot have, which is only observable when the buffer really is that
//     long. TestALengthPastTheCeilingStopsBeforeTheChecksum below stages that,
//     and it fails without the ceiling on any architecture.
//   - it is the only thing between a corrupt length and an integer overflow
//     where int is 32 bits. That is what the last two cases here are for, and
//     they are arch-sensitive: run `GOARCH=386 go test -count=1 .` to see them.
//     Whether they hold depends on the ceiling comparing the on-disk uint32
//     rather than an int converted from it.
func TestAbsurdLengthFieldIsATornTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length uint32
	}{
		{"one byte past the ceiling", maxPayload + 1},
		{"most of a uint32", 0xFFFFFFF0},
		{"the high bit set, which is a negative int where int is 32 bits", 0x80000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, offsets := buildLog(t, []uint64{1, 2})

			// Overwrite record 2's length field. Its checksum will not match
			// either, but the length has to be rejected as a length: by the
			// time the checksum is consulted the length has already been used
			// to slice the buffer.
			binary.LittleEndian.PutUint32(buf[offsets[1]+4:], tc.length)

			got, reason, consumed := collect(buf)

			if reason != stopTornRecord {
				t.Fatalf("a record claiming a payload of %#x stopped replay for reason %q, want %q", tc.length, reason, stopTornRecord)
			}
			if want := []string{keyN(1)}; !equalStrings(appliedKeys(got), want) {
				t.Fatalf("applied %v, want %v", appliedKeys(got), want)
			}
			if consumed != offsets[1] {
				t.Errorf("consumed %d bytes, want %d", consumed, offsets[1])
			}
		})
	}
}

// B3. The ceiling has to fire BEFORE the checksum, not merely somewhere.
//
// Every other length case here is rejected because the buffer is too short to
// hold what the length claims, which is what a torn tail at the end of a
// segment looks like and is not what a merely wrong length looks like. This
// one hands replay a buffer that really is long enough: a seventeen-megabyte
// payload claimed inside a buffer with seventeen megabytes in it. With the
// ceiling gone, crc32 runs over all of it and the record is rejected as a
// checksum mismatch instead - the same stop, one line later, after seventeen
// megabytes of work that one comparison avoids. It is the only assertion in
// this repository that fails when the ceiling is deleted, which is the reason
// it exists: a reviewer deleted those three lines and the entire suite,
// 240-seed corpus included, stayed green.
func TestALengthPastTheCeilingStopsBeforeTheChecksum(t *testing.T) {
	const claimed = maxPayload + 1<<20

	buf, offsets := buildLog(t, []uint64{1, 2})
	binary.LittleEndian.PutUint32(buf[offsets[1]+4:], claimed)
	buf = append(buf, make([]byte, headerSize+claimed)...)

	if len(buf)-offsets[1] < headerSize+claimed {
		t.Fatalf("staging: %d bytes after the start of record 2, need %d - this test is only about the case where the buffer IS long enough", len(buf)-offsets[1], headerSize+claimed)
	}

	got, reason, consumed := collect(buf)

	if reason != stopTornRecord {
		t.Fatalf("a record claiming %d bytes of payload, inside a buffer that has them, stopped replay for reason %q, want %q - the length is past the ceiling and must be rejected as impossible rather than checksummed", claimed, reason, stopTornRecord)
	}
	if want := []string{keyN(1)}; !equalStrings(appliedKeys(got), want) {
		t.Fatalf("applied %v, want %v", appliedKeys(got), want)
	}
	if consumed != offsets[1] {
		t.Errorf("consumed %d bytes, want %d", consumed, offsets[1])
	}
}

// The chain, specifically: a record that is perfectly valid on its own but was
// written after a different predecessor must not verify. This is what stops a
// record copied from elsewhere in the log - or from another store's log
// entirely - from being accepted at the wrong position.
func TestRecordFromAnotherChainIsRejected(t *testing.T) {
	first, _ := buildLog(t, []uint64{1, 2})
	// A second log whose record 2 says the same thing but follows a different
	// record 1, so its chained checksum differs.
	var other []byte
	var crc uint32
	other, crc = appendRecord(other, record{seq: 1, kind: kindPut, key: "different", value: []byte("x")}, crc)
	other, _ = appendRecord(other, record{seq: 2, kind: kindPut, key: keyN(2), value: valueN(2)}, crc)

	_, offsets := buildLog(t, []uint64{1, 2})
	spliced := append(append([]byte(nil), first[:offsets[1]]...), other[len(other)-(len(first)-offsets[1]):]...)

	got, reason, _ := collect(spliced)
	if reason != stopChecksum {
		t.Fatalf("stop reason %q, want %q - a record from another chain must not verify", reason, stopChecksum)
	}
	if len(got) != 1 {
		t.Fatalf("applied %d records, want 1", len(got))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rechainKeyLen rewrites the key-length field of the record starting at `start`
// and recomputes that record's chained checksum so the record still verifies.
//
// The point of a test on keyLen is that the checksum PASSES: keyLen is read
// after the checksum has vouched for the bytes, so a keyLen that disagrees with
// the payload is not corruption in the usual sense. It is either a record
// written by a build this one does not understand, or a length field that was
// scribbled on and then re-checksummed by a crash that happened to land that
// way. Either way decodeRecord has to survive it, and a test that let the
// checksum fail would never reach the line it is about.
//
// `prevCRC` is the checksum the record at `start` chains to - for the second
// record of a log built by buildLog that is the first record's stored crc.
func rechainKeyLen(buf []byte, start int, prevCRC uint32, keyLen uint32) {
	binary.LittleEndian.PutUint32(buf[start+17:], keyLen)
	total := headerSize + int(binary.LittleEndian.Uint32(buf[start+4:]))
	crc := crc32.Update(prevCRC, castagnoli, buf[start+4:start+total])
	binary.LittleEndian.PutUint32(buf[start:], crc)
}

// B3. The key-length field is the second value decodeRecord reads off disk and
// converts to an int, and it needs the same treatment as the first one.
//
// This line was the one line a reviewer could delete with the whole root suite
// still green, which is the definition of an untested guard. It is also the
// same defect as the length field four lines above it: `keyLen > payload`
// compares an int converted from an on-disk uint32, and where int is 32 bits
// `int(0x80000000)` is -2147483648, which is not greater than anything. The
// guard passes and `body[:keyLen]` slices with a negative bound - a panic
// inside recovery, on precisely the input recovery exists to survive.
//
//	amd64:  keyLen = 2147483648   keyLen > payload = true    rejected
//	386:    keyLen = -2147483648  keyLen > payload = false   panic
//
// The first two cases fail on any architecture with the guard removed; the
// third fails only where int is 32 bits, and it is why this test is run under
// `GOARCH=386 go test -count=1 .` as well as under the ordinary gate. B3 says a
// record failing a check ends recovery at that point. A panic is not ending
// recovery.
func TestAKeyLengthPastThePayloadIsATornTail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keyLen uint32
	}{
		{"one byte past the payload", 14},
		{"far past the payload, inside the buffer", 9999},
		{"the high bit set, which is a negative int where int is 32 bits", 0x80000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, offsets := buildLog(t, []uint64{1, 2})
			payload := binary.LittleEndian.Uint32(buf[offsets[1]+4:])
			if tc.keyLen <= payload {
				t.Fatalf("staging: record 2 claims a payload of %d and this case sets keyLen to %d, which is not past it - the case is not testing what it says", payload, tc.keyLen)
			}
			rechainKeyLen(buf, offsets[1], binary.LittleEndian.Uint32(buf[0:]), tc.keyLen)

			got, reason, consumed := collect(buf)

			if reason != stopTornRecord {
				t.Fatalf("a record whose checksum passes and whose key length is %#x stopped replay for reason %q, want %q", tc.keyLen, reason, stopTornRecord)
			}
			if want := []string{keyN(1)}; !equalStrings(appliedKeys(got), want) {
				t.Fatalf("applied %v, want %v - recovery must stop at the last record it understands", appliedKeys(got), want)
			}
			if consumed != offsets[1] {
				t.Errorf("consumed %d bytes, want %d", consumed, offsets[1])
			}
		})
	}
}
