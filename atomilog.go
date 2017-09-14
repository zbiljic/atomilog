// Package atomilog provides a rolling logger.
//
// AtomiLog is intended to be one part of a logging infrastructure.
// It is not an all-in-one solution, but instead is a pluggable component at
// the bottom of the logging stack that simply controls the files to which logs
// are written.
//
// AtomiLog plays well with any logging package that can write to an io.Writer,
// including the standard library's log package.
//
// AtomiLog is build to support multiple processes on the same machine that are
// writing to the same log files.
package atomilog

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	filelock "github.com/zbiljic/go-filelock"
)

const (
	backupTimeFormat     = "2006-01-02T15-04-05.000"
	defaultMaxFileSizeMB = 100
	defaultMaxBackups    = 10
	defaultMaxBacklog    = 128
	lockPathFormat       = "%s.lock"
)

var (
	// currentTime exists so it can be mocked out by tests.
	currentTime = time.Now

	// megabyte is the conversion factor between maxFileSize and bytes.  It is a
	// variable so tests can mock it out and not need to write megabytes of data
	// to disk.
	megabyte = 1024 * 1024
)

// Logger is an io.WriteCloser that writes to the specified filename.
//
// Logger opens or creates the logfile on first Write.  If the file exists and
// is less than maxFileSize bytes, AtomiLog will open and append to that file.
// If the file exists and its size is >= maxFileSize bytes, the file is renamed
// by putting the current time in a timestamp in the name immediately before the
// file's extension (or the end of the filename if there's no extension). A new
// log file is then created using original filename.
//
// Whenever a write causes the current log file to exceed maxFileSize bytes,
// the current file is closed, renamed, and a new log file created with the
// original name. Thus, the filename you give Logger is always the "current" log
// file.
//
// File rotation is performed while holding global system lock, so multiple
// processes can write to the same file. When the time comes to rotate the
// current logfile, first process that successfully acquired the lock will
// perform the actual rotation, other ones will reopen the new logfile and
// use it.
//
// Cleaning Up Old Log Files
//
// Whenever a new logfile gets created, old log files may be deleted.  The most
// recent files according to the encoded timestamp will be retained, up to a
// number equal to maxBackups (or all of them if maxBackups is 0).  Note that
// the time encoded in the timestamp is the rotation time, which may differ
// from the last time that file was written to.
type Logger struct {
	// filename is the file to write logs to.  Backup log files will be retained
	// in the same directory.
	filename string

	// maxFileSize is the maximum size in bytes of the log file before it gets
	// rotated. It defaults to 100 megabytes.
	maxFileSize int64

	// maxBackups is the maximum number of old log files to retain.  The default
	// is to retain 10 old log files.
	maxBackups int

	// maxBacklog is the maximum number of elements in the channel that holds the
	// write entries until they are written to the log file by some background
	// process.
	maxBacklog int

	// backlog is the channel that holds write entries until they are written to
	// the log file by some background process.
	backlog chan []byte

	// closed is used to prevent any further writes after the Logger is closed.
	closed chan struct{}

	// lockFilename is the file which will be used to lock on attempt to rotate
	// the log file.
	lockFilename string

	queuedCount  uint64
	writtenCount uint64

	file *os.File
	emu  sync.Mutex
	mu   sync.Mutex
}

// Option is used to pass optional arguments to the Logger constructor
type Option interface {
	configure(*Logger) error
}

// OptionFn is a type of Option that is represented by a single function that
// gets called for configure()
type OptionFn func(*Logger) error

func (o OptionFn) configure(l *Logger) error {
	return o(l)
}

// WithMaxFileSize creates a new Option that sets the max size of a log file
// before it gets rotated.
func WithMaxFileSize(maxFileSize int64) Option {
	return OptionFn(func(l *Logger) error {
		l.maxFileSize = maxFileSize
		return nil
	})
}

// WithMaxFileSizeMB creates a new Option that sets the max size of a log file
// before it gets rotated.
func WithMaxFileSizeMB(maxFileSizeMB int) Option {
	return OptionFn(func(l *Logger) error {
		l.maxFileSize = int64(maxFileSizeMB) * int64(megabyte)
		return nil
	})
}

// WithMaxBackups creates a new Option that sets the max backups of log files.
func WithMaxBackups(maxBackups int) Option {
	return OptionFn(func(l *Logger) error {
		l.maxBackups = maxBackups
		return nil
	})
}

// WithMaxBacklog creates a new Option that sets the channel backlog for
// buffered log messages.
func WithMaxBacklog(maxBacklog int) Option {
	return OptionFn(func(l *Logger) error {
		l.maxBacklog = maxBacklog
		return nil
	})
}

// New returns pointer to a new Logger object.
func New(filename string, options ...Option) (*Logger, error) {
	if !filepath.IsAbs(filename) {
		return nil, errors.New("absolute filename name must be provided")
	}

	var l Logger
	l.filename = filename
	l.maxFileSize = int64(defaultMaxFileSizeMB * megabyte)
	l.maxBackups = defaultMaxBackups
	l.maxBacklog = defaultMaxBacklog
	for _, opt := range options {
		opt.configure(&l)
	}
	l.backlog = make(chan []byte, l.maxBacklog)
	l.closed = make(chan struct{}, 1)
	l.lockFilename = fmt.Sprintf(lockPathFormat, filename)

	// process write entries in the background
	go l.processBacklog()

	return &l, nil
}

// Write implements io.Writer.
//
// If the length of the write is greater than maxFileSize, an error is returned.
func (l *Logger) Write(b []byte) (n int, err error) {

	writeLen := len(b)
	if writeLen == 0 {
		return 0, nil
	}
	if int64(writeLen) > l.maxFileSize {
		return 0, fmt.Errorf(
			"write length %d exceeds maximum file size %d", writeLen, l.maxFileSize)
	}

	// we MUST copy byte slice
	tb := make([]byte, writeLen)
	copy(tb, b)

	select {
	case <-l.closed:
		// Prevent further writes
		err = fmt.Errorf("logger is closed")
		l.closed <- struct{}{}
		break
	case l.backlog <- tb:
		// Will block until written
		n = int(writeLen)
		// increment queue count
		atomic.AddUint64(&l.queuedCount, 1)
	}

	return n, err
}

// Close satisfies the io.Closer interface. You must call this method if you
// performed any writes to the object.
func (l *Logger) Close() error {
	l.emu.Lock()
	defer l.emu.Unlock()

	// signal closed
	l.closed <- struct{}{}
	// flush remaining writes
	l.flush()
	// close backlog channel
	close(l.backlog)

	// internal lock
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.closeFile()
}

// Flush forces all backlog content to be written to the backing file.
func (l *Logger) Flush() error {
	l.emu.Lock()
	defer l.emu.Unlock()

	return l.flush()
}

// Rotate causes Logger to close the existing log file and immediately create a
// new one.  This is a helper function for applications that want to initiate
// rotations outside of the normal rotation rules, such as in response to
// SIGHUP.  After rotating, this initiates a cleanup of old log files according
// to the normal rules.
func (l *Logger) Rotate() error {
	l.emu.Lock()
	defer l.emu.Unlock()

	// internal lock
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.rotate(true)
}

// ioWrite opens the configured file, and appends the data to it.
//
// LOCKS_EXCLUDED(l.mu)
func (l *Logger) ioWrite(b []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		if err = l.openExistingOrNew(); err != nil {
			return 0, err
		}
	}

	info, err := l.file.Stat() // syscall.Fstat
	if err != nil {
		return 0, fmt.Errorf("error getting log file info: %s", err)
	}
	fileSize := info.Size()

	if fileSize > int64(l.maxFileSize) {
		if err = l.rotate(false); err != nil {
			return 0, err
		}
	}

	n, err = l.file.Write(b) // syscall.Write

	if n > maxWriteSize {
		l.file.Sync() // syscall.Fsync
	}

	// increment write count
	atomic.AddUint64(&l.writtenCount, 1)

	return n, err
}

// dir returns the directory for the current filename.
func (l *Logger) dir() string {
	return filepath.Dir(l.filename)
}

// closeFile closes the file if it is open.
//
// LOCKS_REQUIRED(l.mu)
func (l *Logger) closeFile() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil

	return err
}

// rotate closes the current file, moves it aside with a timestamp in the name,
// (if it exists), opens a new file with the original filename, and then runs
// cleanup.
//
// LOCKS_REQUIRED(l.mu)
func (l *Logger) rotate(force bool) error {
	if err := l.closeFile(); err != nil {
		return err
	}
	if err := l.openNew(force); err != nil {
		return err
	}
	return l.cleanup()
}

// openNew opens a new log file for writing, moving any old log file out of the
// way.  This methods assumes the file has already been closed.
//
// LOCKS_REQUIRED(l.mu)
func (l *Logger) openNew(force bool) error {
	err := os.MkdirAll(l.dir(), 0744)
	if err != nil {
		return fmt.Errorf("can't make directories for new logfile: %s", err)
	}

	filename := l.filename
	mode := os.FileMode(0644)
	info, err := os.Stat(filename) // syscall.Stat
	if err == nil {
		// Copy the mode off the old logfile.
		mode = info.Mode()
		// move the existing file
		newname := backupName(filename, false)

		tryRotate := true

		// fast path
		if !force {
			filesize := info.Size()
			if filesize < l.maxFileSize {
				tryRotate = false
			}
		}

		if tryRotate {
			// get the lock
			var lock filelock.TryLockerSafe
			lock, err = filelock.New(l.lockFilename)
			if err != nil {
				return err
			}

			var locked bool
			// stop trying after some time
			for index := 0; index < 10; index++ {
				locked, err = lock.TryLock()
				if err != nil && err != filelock.ErrLocked {
					return err
				}
				if locked {
					// we have a lock
					defer lock.Unlock()

					// recheck the file
					var newInfo os.FileInfo
					newInfo, err = os.Stat(filename) // syscall.Stat
					if err != nil {
						return err
					}
					newFilesize := newInfo.Size()

					if !force {
						if newFilesize < l.maxFileSize {
							// someone else rotated the file
							break
						}
					}

					// we rename the file
					if err = os.Rename(filename, newname); err != nil { // syscall [3]
						return fmt.Errorf("can't rename log file: %s", err)
					}

					// this is a no-op anywhere but linux
					if err = chown(filename, info); err != nil { // syscall [3]
						return err
					}

					// we rotated the file
					break

				} else {
					// should be enough time for someone to rename the file
					time.Sleep(100 * time.Millisecond)
				}
			}
		}

	}

	// we use append here because this should work across processes
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND|os.O_CREATE, mode)
	if err != nil {
		return fmt.Errorf("can't open new logfile: %s", err)
	}
	l.file = file

	return nil
}

// backupName creates a new filename from the given name, inserting a timestamp
// between the filename and the extension, using the local time if requested
// (otherwise UTC).
func backupName(name string, local bool) string {
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	t := currentTime()
	if !local {
		t = t.UTC()
	}

	timestamp := t.Format(backupTimeFormat)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestamp, ext))
}

// openExistingOrNew opens the logfile if it exists.  If there is no such file
// a new file is created. It is possible that log file size becomes greater than
// maxFileSize.
//
// LOCKS_REQUIRED(l.mu)
func (l *Logger) openExistingOrNew() error {
	info, err := os.Stat(l.filename) // syscall.Stat
	if os.IsNotExist(err) {
		return l.openNew(false)
	}
	if err != nil {
		return fmt.Errorf("error getting log file info: %s", err)
	}

	if info.Size() >= int64(l.maxFileSize) {
		return l.rotate(false)
	}

	file, err := os.OpenFile(l.filename, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		// if we fail to open the old log file for some reason, just ignore
		// it and open a new log file.
		return l.openNew(false)
	}
	l.file = file

	return nil
}

// flush waits until all queued messages have been written by the background
// process.
func (l *Logger) flush() error {

	currentQueuedCount := atomic.LoadUint64(&l.queuedCount)
	currentWrittenCount := atomic.LoadUint64(&l.writtenCount)

	// slow, finite loop
	for index := 0; index < l.maxBacklog; index++ {
		if currentWrittenCount >= currentQueuedCount {
			break
		}
		time.Sleep(100 * time.Millisecond)
		currentWrittenCount = atomic.LoadUint64(&l.writtenCount)
	}

	return nil
}

func (l *Logger) processBacklog() {
	for {
		select {
		case b, open := <-l.backlog:
			// process backlog, will block
			if !open {
				return
			}
			l.ioWrite(b)
			break
		}
	}
}

// cleanup deletes old log files, keeping at most l.maxBackups files
//
// LOCKS_REQUIRED(l.mu)
func (l *Logger) cleanup() error {

	files, err := l.oldLogFiles()
	if err != nil {
		return err
	}

	var deletes []logInfo

	if l.maxBackups > 0 && l.maxBackups < len(files) {
		deletes = files[l.maxBackups:]
		files = files[:l.maxBackups]
	}

	if len(deletes) == 0 {
		return nil
	}

	go deleteAll(l.dir(), deletes)

	return nil
}

func deleteAll(dir string, files []logInfo) {
	// remove files on a separate goroutine
	for _, f := range files {
		// what am I going to do, log this?
		_ = os.Remove(filepath.Join(dir, f.Name()))
	}
}

// oldLogFiles returns the list of backup log files stored in the same
// directory as the current log file, sorted by ModTime
func (l *Logger) oldLogFiles() ([]logInfo, error) {
	files, err := ioutil.ReadDir(l.dir())
	if err != nil {
		return nil, fmt.Errorf("can't read log file directory: %s", err)
	}
	logFiles := []logInfo{}

	prefix, ext := l.prefixAndExt()

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := l.timeFromName(f.Name(), prefix, ext)
		if name == "" {
			continue
		}
		t, err := time.Parse(backupTimeFormat, name)
		if err == nil {
			logFiles = append(logFiles, logInfo{t, f})
		}
		// error parsing means that the suffix at the end was not generated
		// by AtomiLog, and therefore it's not a backup file.
	}

	sort.Sort(byFormatTime(logFiles))

	return logFiles, nil
}

// timeFromName extracts the formatted time from the filename by stripping off
// the filename's prefix and extension. This prevents someone's filename from
// confusing time.parse.
func (l *Logger) timeFromName(filename, prefix, ext string) string {
	if !strings.HasPrefix(filename, prefix) {
		return ""
	}
	filename = filename[len(prefix):]

	if !strings.HasSuffix(filename, ext) {
		return ""
	}
	filename = filename[:len(filename)-len(ext)]
	return filename
}

// prefixAndExt returns the filename part and extension part from the Logger's
// filename.
func (l *Logger) prefixAndExt() (prefix, ext string) {
	filename := filepath.Base(l.filename)
	ext = filepath.Ext(filename)
	prefix = filename[:len(filename)-len(ext)] + "-"
	return prefix, ext
}

// logInfo is a convenience struct to return the filename and its embedded
// timestamp.
type logInfo struct {
	timestamp time.Time
	os.FileInfo
}

// byFormatTime sorts by newest time formatted in the name.
type byFormatTime []logInfo

func (b byFormatTime) Less(i, j int) bool {
	return b[i].timestamp.After(b[j].timestamp)
}

func (b byFormatTime) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

func (b byFormatTime) Len() int {
	return len(b)
}

// Check the interfaces are satisfied
var (
	_ io.WriteCloser = &Logger{}
	_ sort.Interface = &byFormatTime{}
)
