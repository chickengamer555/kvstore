package kvstore

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

// The on-disk record.
//
//	offset  size  field
//	------  ----  -----------------------------------------------------------
//	     0     4  crc32c, chained: Update(previous record's crc, bytes [4:end])
//	     4     4  payload length (key + value)
//	     8     8  sequence number
//	    16     1  kind
//	    17     4  key length
//	    21     n  key bytes, then value bytes
//
// Two things about this layout are load-bearing and were not obvious to me
// when I started.
//
// First, the checksum covers the length field. It has to: if it did not, a
// crash that scribbles over the length turns a torn tail into a plausible-
// looking record of the wrong size, and everything after it decodes as
// garbage that passes its own checks. Covering the length means a corrupt
// length is caught - but only after we have read that many bytes, which is
// why maxPayload exists below.
//
// Second, the checksum is *chained*: a record's crc is seeded with the
// previous record's crc rather than with zero. That is what makes the
// sequence number a chain rather than a counter. A record lifted from
// somewhere else in the log, or from a different log, has the right internal
// structure and still fails, because its predecessor is not the one it was
// written after.
const (
	headerSize = 21

	// A payload larger than this is treated as end-of-log rather than as an
	// allocation request. Any suffix of the log can be garbage after a crash,
	// and a garbage length prefix that says 3.9GB must not become a 3.9GB
	// make([]byte). 16MiB is far above any value this store will write and far
	// below anything that hurts.
	maxPayload = 16 << 20
)

type recordKind uint8

const (
	kindPut    recordKind = 1
	kindDelete recordKind = 2
)

type record struct {
	seq   uint64
	kind  recordKind
	key   string
	value []byte
}

// crc32c. Castagnoli rather than IEEE because it has hardware support on both
// amd64 and arm64, and this runs on every record on both the write and the
// recovery path.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// stopReason says why recovery stopped reading a segment. Recovery always
// stops at the first record it cannot vouch for; the reason is kept because
// "the log simply ended" and "the log is corrupt" look identical in the data
// and mean very different things to whoever is reading the report.
type stopReason string

const (
	stopEndOfLog   stopReason = "end-of-log"
	stopTornRecord stopReason = "torn-record"
	stopChecksum   stopReason = "checksum-mismatch"
	stopSequence   stopReason = "sequence-break"
)

var (
	errTornRecord = errors.New("kvstore: record truncated")
	errChecksum   = errors.New("kvstore: record checksum mismatch")
	errSequence   = errors.New("kvstore: record sequence break")
)

func reasonFor(err error) stopReason {
	switch {
	case errors.Is(err, errChecksum):
		return stopChecksum
	case errors.Is(err, errSequence):
		return stopSequence
	default:
		return stopTornRecord
	}
}

// appendRecord encodes r onto dst, chaining its checksum to prevCRC, and
// returns the grown slice together with this record's checksum - which is what
// the next record must chain to.
func appendRecord(dst []byte, r record, prevCRC uint32) ([]byte, uint32) {
	start := len(dst)
	payload := len(r.key) + len(r.value)

	dst = append(dst, make([]byte, headerSize)...)
	binary.LittleEndian.PutUint32(dst[start+4:], uint32(payload))
	binary.LittleEndian.PutUint64(dst[start+8:], r.seq)
	dst[start+16] = byte(r.kind)
	binary.LittleEndian.PutUint32(dst[start+17:], uint32(len(r.key)))
	dst = append(dst, r.key...)
	dst = append(dst, r.value...)

	crc := crc32.Update(prevCRC, castagnoli, dst[start+4:])
	binary.LittleEndian.PutUint32(dst[start:], crc)
	return dst, crc
}

// decodeRecord reads one record from the front of buf.
//
// It returns the record, how many bytes it consumed, and the checksum the next
// record must chain to. Every failure mode - too few bytes, an implausible
// length, a bad checksum, a sequence that does not follow - is an error, and
// every one of them means recovery stops here. None of them means "skip this
// record and carry on": a gap in the chain is exactly the shape of the bug
// this design exists to make impossible to miss.
func decodeRecord(buf []byte, prevCRC uint32, wantSeq uint64) (record, int, uint32, error) {
	// Not implemented yet - see record_test.go for what this has to do.
	return record{}, 0, prevCRC, errTornRecord
}
