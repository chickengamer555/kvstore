//go:build windows

package kvstore

// On Windows this is a no-op, and the no-op is correct rather than a gap.
//
// The POSIX pattern - open the containing directory, fsync(2) that descriptor -
// has no Windows equivalent. A directory cannot be opened as a file and handed
// to FlushFileBuffers; the call the platform gives you flushes a file's data
// buffers, not a directory's metadata.
//
// It is not needed either, and that is the part worth understanding. NTFS
// journals metadata operations: the creation of a directory entry is written to
// the $LogFile transaction log before the operation is reported complete, and
// the entry is recovered from that journal after a crash. The durability of the
// name is the filesystem's responsibility. On ext4 with the default
// data=ordered mode it is the application's, which is why the POSIX build does
// the extra fsync and this one does not.
//
// So the store does reach the decision on Windows - it records that it reached
// it, and reports the guarantee as coming from the filesystem rather than from
// this code. What it must never do is stay silent and let a README written
// against Linux imply the same proof holds here. Nothing in this file has been
// verified by the crash corpus; the corpus runs on Linux.
const dirSyncSupported = false

const dirSyncNote = "windows/NTFS: no directory fsync is issued and none exists. NTFS journals metadata operations through $LogFile, so a new file's directory entry is made durable by the filesystem rather than by this code. This has not been verified here - the crash corpus runs on Linux."

func syncDir(_ string) error { return nil }
