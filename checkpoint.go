package kvstore

import (
	"encoding/binary"
	"hash/crc32"
	"path/filepath"
)

// A checkpoint is the live key set, serialised, plus the sequence number it was
// taken at. Once it is durable, every log record up to that sequence number is
// redundant and the segments holding them can be deleted - which is the only
// thing that bounds the log.
//
//	offset  size  field
//	     0     8  magic
//	     8     4  crc32c over everything from offset 12
//	    12     8  sequence number this checkpoint covers
//	    20     n  serialised state (see state.go)
const (
	checkpointName       = "CHECKPOINT"
	checkpointTempName   = "CHECKPOINT.tmp"
	checkpointHeaderSize = 20
)

var checkpointMagic = []byte("KVCKPT\x00\x01")

func checkpointChecksum(buf []byte) uint32 {
	return crc32.Checksum(buf[12:], castagnoli)
}

type checkpoint struct {
	seq  uint64
	data map[string][]byte
}

// loadCheckpoint reads the checkpoint file if there is a usable one.
//
// "Usable" is doing a lot of work. A crash can leave this file half written, so
// it is exactly as trustworthy as a log record and gets the same treatment: a
// bad magic, a bad checksum or a payload that does not decode means there is no
// checkpoint, not that the store is broken. The log is still complete, because
// segments are only deleted after the checkpoint replacing them is durable.
//
// The second return value distinguishes "there was no checkpoint" from "there
// was one and it was rejected", because those look identical in the recovered
// state and are very different things to see in a report.
//
// Not implemented yet - checkpoint_test.go says what this has to do.
func loadCheckpoint(_ string) (*checkpoint, bool) { return nil, false }

// writeCheckpoint makes a checkpoint durable, then makes its name durable.
//
// Not implemented yet.
func writeCheckpoint(_ string, _ uint64, _ []byte) error { return nil }

func checkpointPath(dir string) string     { return filepath.Join(dir, checkpointName) }
func checkpointTempPath(dir string) string { return filepath.Join(dir, checkpointTempName) }

func encodeCheckpoint(seq uint64, payload []byte) []byte {
	buf := make([]byte, checkpointHeaderSize, checkpointHeaderSize+len(payload))
	copy(buf, checkpointMagic)
	binary.LittleEndian.PutUint64(buf[12:], seq)
	buf = append(buf, payload...)
	binary.LittleEndian.PutUint32(buf[8:], checkpointChecksum(buf))
	return buf
}
