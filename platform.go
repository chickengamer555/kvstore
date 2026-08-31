package kvstore

import "runtime"

// Guarantees is what this build actually promises, on the platform it was
// compiled for.
//
// It exists because durability is the one property where a library must not
// let its documentation speak for it. The README is written by someone who was
// looking at Linux; the binary might be running somewhere else, or might have
// been built with a flag that turned a guarantee off. This is the store
// answering the question itself, at runtime, for the build that is actually
// running.
type Guarantees struct {
	// Platform is runtime.GOOS.
	Platform string

	// AckAfterSync is true when no Put returns before its record's fsync has
	// returned. False means this build acknowledges earlier than that, which
	// is only ever the deliberately broken build the crash harness uses as a
	// negative control.
	AckAfterSync bool

	// DirSync is true when the store fsyncs the directory containing a newly
	// created log segment or checkpoint. False does not necessarily mean the
	// directory entry is not durable - see DirSyncNote, which says which it is.
	DirSync bool

	// DirSyncNote states, in words, why this platform does or does not need
	// the application to sync the containing directory.
	DirSyncNote string
}

// Platform reports the guarantees of this build.
func Platform() Guarantees {
	return Guarantees{
		Platform:     runtime.GOOS,
		AckAfterSync: ackAfterSync,
		DirSync:      dirSyncSupported,
		DirSyncNote:  dirSyncNote,
	}
}
