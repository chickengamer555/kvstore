package kvstore

import (
	"bytes"
	"encoding/binary"
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
// allocate that many bytes.
func TestAbsurdLengthFieldIsATornTail(t *testing.T) {
	buf, offsets := buildLog(t, []uint64{1, 2})

	// Overwrite record 2's length field with something absurd. The checksum
	// will not match either, but the length check must fire first - otherwise
	// verifying the checksum means reading 4GB that is not there.
	binary.LittleEndian.PutUint32(buf[offsets[1]+4:], 0xFFFFFFF0)

	got, reason, consumed := collect(buf)

	if reason != stopTornRecord {
		t.Fatalf("stop reason %q, want %q", reason, stopTornRecord)
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
