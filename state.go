package kvstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// The serialised form of a store's live key set.
//
//	uvarint  number of pairs
//	repeated, in ascending byte order of key:
//	  uvarint  key length
//	  bytes    key
//	  uvarint  value length
//	  bytes    value
//
// Sorting is the whole point. Go randomises map iteration order on purpose, so
// anything that walks the map produces different bytes on every run inside a
// single process - which would make "two replays produce identical state"
// impossible to assert and, worse, make it look like a bug in recovery when it
// was a bug in the comparison.
//
// This format is used for two things: the byte-for-byte determinism comparison
// in clause B4, and the payload of a checkpoint file.
func encodeState(data map[string][]byte) []byte {
	keys := make([]string, 0, len(data))
	size := 0
	for k := range data {
		keys = append(keys, k)
		size += len(k) + len(data[k]) + 4
	}
	sort.Strings(keys)

	buf := make([]byte, 0, size+8)
	buf = binary.AppendUvarint(buf, uint64(len(keys)))
	for _, k := range keys {
		buf = binary.AppendUvarint(buf, uint64(len(k)))
		buf = append(buf, k...)
		v := data[k]
		buf = binary.AppendUvarint(buf, uint64(len(v)))
		buf = append(buf, v...)
	}
	return buf
}

var errBadState = errors.New("kvstore: malformed serialised state")

// decodeState is the inverse. Every length is checked against what is actually
// left in the buffer, because this is also the checkpoint decoder and a
// checkpoint file is exactly as trustworthy as a log record: a crash can leave
// it half written.
func decodeState(buf []byte) (map[string][]byte, error) {
	n, used := binary.Uvarint(buf)
	if used <= 0 {
		return nil, fmt.Errorf("%w: bad pair count", errBadState)
	}
	buf = buf[used:]

	out := make(map[string][]byte, n)
	for i := uint64(0); i < n; i++ {
		k, rest, err := takeBytes(buf)
		if err != nil {
			return nil, fmt.Errorf("%w: pair %d key: %v", errBadState, i, err)
		}
		v, rest, err := takeBytes(rest)
		if err != nil {
			return nil, fmt.Errorf("%w: pair %d value: %v", errBadState, i, err)
		}
		out[string(k)] = append([]byte(nil), v...)
		buf = rest
	}
	if len(buf) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", errBadState, len(buf))
	}
	return out, nil
}

func takeBytes(buf []byte) ([]byte, []byte, error) {
	n, used := binary.Uvarint(buf)
	if used <= 0 {
		return nil, nil, errors.New("bad length prefix")
	}
	buf = buf[used:]
	if n > uint64(len(buf)) {
		return nil, nil, fmt.Errorf("length %d exceeds the %d bytes remaining", n, len(buf))
	}
	return buf[:n], buf[n:], nil
}
