package atomilog_test

import (
	"log"

	"github.com/zbiljic/atomilog"
)

// To use AtomiLog with the standard library's log package, just pass it into
// the SetOutput function when your application starts.
func Example() {
	alog, _ := atomilog.New(
		"/path/to/access.log",
		atomilog.WithMaxFileSize(4096), // 4kb
		atomilog.WithMaxBackups(2),
		atomilog.WithMaxBacklog(4),
	)
	log.SetOutput(alog)
}
