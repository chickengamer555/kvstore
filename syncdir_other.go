//go:build !unix && !windows

package kvstore

// Everything that is neither POSIX nor Windows - js/wasm, plan9, wasip1.
//
// I have not established what directory-entry durability means on any of them,
// so this build reports the guarantee as unavailable and says exactly that.
// Reporting it as available would be a claim I cannot back; refusing to build
// would be worse, because the store is still perfectly usable as a log-backed
// map on those targets provided nobody believes something stronger.
const dirSyncSupported = false

const dirSyncNote = "unrecognised platform: no directory fsync is issued, and the author has not established what directory-entry durability means here. Treat the durability claims as unverified on this target."

func syncDir(_ string) error { return nil }
