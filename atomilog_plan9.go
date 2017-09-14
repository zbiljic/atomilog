package atomilog

// maxWriteSize represents the largest size of a single file write operation
// supported that should be atomic
//
// see http://ar.to/notes/posix
const (
	maxWriteSize = 8192
)
