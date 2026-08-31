package kvstore

// replayBytes walks the records in buf, in order, handing each one to apply.
//
// It stops at the first record it cannot vouch for and reports why. The
// important property is that it never skips: a record that fails its checksum
// or breaks the sequence chain ends the replay, and nothing after it is read.
// Skipping the bad record and carrying on would let a crash that scrambled the
// middle of the log resurrect writes that were made after the ones it lost,
// which is a far worse outcome than losing the tail.
//
// startCRC is the checksum of the record before the first one in buf, and
// wantSeq is the sequence number that record must carry. Together they are the
// link that makes a lifted or reordered record fail.
//
// Returns the checksum and sequence number of the last accepted record, how
// many bytes were consumed, and the reason it stopped.
func replayBytes(buf []byte, startCRC uint32, wantSeq uint64, apply func(record)) (uint32, uint64, int, stopReason) {
	crc := startCRC
	seq := wantSeq - 1
	off := 0

	for off < len(buf) {
		r, n, next, err := decodeRecord(buf[off:], crc, seq+1)
		if err != nil {
			return crc, seq, off, reasonFor(err)
		}
		apply(r)
		crc = next
		seq = r.seq
		off += n
	}
	return crc, seq, off, stopEndOfLog
}
