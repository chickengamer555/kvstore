//go:build unix

package kvstore

import "os"

// On a POSIX filesystem the application owns directory-entry durability.
//
// Creating a file and fsyncing it makes the file's *contents* durable. It does
// not make the directory entry that names the file durable: that entry lives in
// the parent directory's own metadata, and on ext4 with the default
// data=ordered it can still be sitting in the journal, unwritten, when the
// power goes. The file survives with no name, which is indistinguishable from
// the file never having existed.
//
// The fix is fsync(2) on a descriptor for the containing directory, which is
// what this does. It has to happen after the file is created and before
// anything is acknowledged on the strength of it.
const dirSyncSupported = true

const dirSyncNote = "POSIX: the containing directory is fsynced after a log segment or checkpoint is created, because a newly created file's directory entry is not durable until the directory itself is synced."

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		// Close and report the sync error, not the close error: the sync
		// failing is the one that means the caller's durability assumption is
		// wrong.
		cerr := d.Close()
		if cerr != nil {
			return err
		}
		return err
	}
	return d.Close()
}
