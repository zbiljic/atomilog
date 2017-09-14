package atomilog_test

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/zbiljic/atomilog"
)

func TestSatisfiesIOWriter(t *testing.T) {
	var w io.Writer
	w, _ = atomilog.New("/foo/bar")
	_ = w
}

func TestSatisfiesIOCloser(t *testing.T) {
	var c io.Closer
	c, _ = atomilog.New("/foo/bar")
	_ = c
}

func TestLogWrite(t *testing.T) {
	dir, err := ioutil.TempDir("", "file-atomilog-test")
	if !assert.NoError(t, err, "creating temporary directory should succeed") {
		return
	}
	defer os.RemoveAll(dir)

	// Change current time, so we can safely purge old logs
	dummyTime := time.Now().Add(-7 * 24 * time.Hour)
	dummyTime = dummyTime.Add(time.Duration(-1 * dummyTime.Nanosecond()))

	filename := filepath.Join(dir, "test.log")

	alog, err := atomilog.New(
		filename,
		atomilog.WithMaxFileSize(4096), // 4kb
		atomilog.WithMaxBackups(2),
		atomilog.WithMaxBacklog(4),
	)
	if !assert.NoError(t, err, `atomilog.New should succeed`) {
		return
	}
	defer alog.Close()

	str := "Hello, World"
	n, err := alog.Write([]byte(str))
	if !assert.NoError(t, err, "alog.Write should succeed") {
		return
	}

	if !assert.Len(t, str, n, "alog.Write should succeed") {
		return
	}

	// Must call `Flush` as writes are asynchronous
	alog.Flush()

	content, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Errorf("Failed to read file %s: %s", filename, err)
	}

	if string(content) != str {
		t.Errorf(`File content does not match (was "%s")`, content)
	}

	err = os.Chtimes(filename, dummyTime, dummyTime)
	if err != nil {
		t.Errorf("Failed to change access/modification times for %s: %s", filename, err)
	}

	fi, err := os.Stat(filename)
	if err != nil {
		t.Errorf("Failed to stat %s: %s", filename, err)
	}

	if !fi.ModTime().Equal(dummyTime) {
		t.Errorf("Failed to chtime for %s (expected %s, got %s)", filename, fi.ModTime(), dummyTime)
	}

	err = alog.Rotate()
	if !assert.NoError(t, err, "alog.Rotate should succeed") {
		return
	}

	content, err = ioutil.ReadFile(filename)
	if err != nil {
		t.Errorf("Failed to read file %s: %s", filename, err)
	}

	if string(content) == str {
		t.Errorf(`File content matches (was "%s")`, content)
	}
}
