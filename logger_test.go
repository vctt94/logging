package logging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/slog"
)

func boolPtr(v bool) *bool { return &v }

func testBackend(t *testing.T, config LogConfig) *LogBackend {
	t.Helper()
	if config.UseStdout == nil {
		config.UseStdout = boolPtr(false)
	}
	b, err := NewLogBackend(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestStructuredRecordAndLegacyCallbacks(t *testing.T) {
	var raw, legacyError string
	var records []LogRecord
	b := testBackend(t, LogConfig{
		MaxBufferLines: 1,
		LogCallback:    func(line string) { raw = line },
		RecordCallback: func(record LogRecord) { records = append(records, record) },
		ErrorCallback:  func(line string) { legacyError = line },
	})
	b.Logger("TEST").Error("structured message")
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	record := records[0]
	if record.Subsystem != "TEST" || record.Level != slog.LevelError || record.Message != "structured message" {
		t.Fatalf("unexpected record: %#v", record)
	}
	if record.Timestamp.IsZero() {
		t.Fatal("record timestamp is zero")
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} \[ERR] TEST: structured message\n$`).MatchString(record.Formatted) {
		t.Fatalf("unexpected formatted record %q", record.Formatted)
	}
	if raw != record.Formatted {
		t.Fatalf("raw callback got %q, want %q", raw, record.Formatted)
	}
	if legacyError != "[ERR] TEST: structured message\n" {
		t.Fatalf("unexpected legacy error callback value %q", legacyError)
	}
	if got := b.LastLogLines(1); len(got) != 1 || got[0] != record.Formatted {
		t.Fatalf("unexpected recent logs: %#v", got)
	}
}

func TestCallbackReentryAndPanicRecovery(t *testing.T) {
	var records []string
	var callbackErrors []error
	var mu sync.Mutex
	var b *LogBackend
	b = testBackend(t, LogConfig{
		LogCallback: func(string) { panic("legacy callback panic") },
		RecordCallback: func(record LogRecord) {
			mu.Lock()
			records = append(records, record.Message)
			mu.Unlock()
			if record.Message == "outer" {
				b.Logger("TEST").Info("inner")
			}
		},
		BackendErrorCallback: func(err error) {
			mu.Lock()
			callbackErrors = append(callbackErrors, err)
			mu.Unlock()
		},
	})
	done := make(chan struct{})
	go func() { b.Logger("TEST").Info("outer"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback re-entry deadlocked")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(records, ","); got != "outer,inner" {
		t.Fatalf("record order %q", got)
	}
	if len(callbackErrors) != 2 {
		t.Fatalf("got %d callback errors, want 2", len(callbackErrors))
	}
}

func TestSinkErrorsDoNotSuppressOtherDestinations(t *testing.T) {
	var stdout bytes.Buffer
	var reported []error
	b := testBackend(t, LogConfig{
		UseStdout:            boolPtr(true),
		MaxBufferLines:       1,
		BackendErrorCallback: func(err error) { reported = append(reported, err) },
	})
	b.stdout = failingWriter{err: errors.New("stdout failed")}
	file := &memorySink{}
	b.rotator = file
	b.Logger("TEST").Info("still buffered")
	if !strings.Contains(file.String(), "still buffered") {
		t.Fatal("file sink did not receive record")
	}
	if len(b.LastLogLines(1)) != 1 {
		t.Fatal("buffer did not receive record")
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "stdout failed") {
		t.Fatalf("unexpected errors: %v", reported)
	}
	// Verify a failed file sink still permits stdout output.
	b.rotator = failingSink{writeErr: errors.New("file failed")}
	b.stdout = &stdout
	b.Logger("TEST").Info("stdout survives")
	if !strings.Contains(stdout.String(), "stdout survives") {
		t.Fatal("stdout did not receive record after file failure")
	}
}

func TestErrorFallbackAndCloseError(t *testing.T) {
	b := testBackend(t, LogConfig{UseStdout: boolPtr(true)})
	var stderr bytes.Buffer
	b.stderr = &stderr
	b.stdout = failingWriter{err: errors.New("broken stdout")}
	b.Logger("TEST").Info("x")
	if !strings.Contains(stderr.String(), "broken stdout") {
		t.Fatalf("stderr fallback missing error: %q", stderr.String())
	}
	b.rotator = failingSink{closeErr: errors.New("close failed")}
	if err := b.Close(); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("unexpected close error %v", err)
	}
	if err := b.Close(); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("repeat close did not return cached error: %v", err)
	}

	b = testBackend(t, LogConfig{
		UseStdout: boolPtr(true),
		BackendErrorCallback: func(error) {
			panic("error callback panic")
		},
	})
	stderr.Reset()
	b.stderr = &stderr
	b.stdout = failingWriter{err: errors.New("another broken stdout")}
	b.Logger("TEST").Info("x")
	if !strings.Contains(stderr.String(), "logging error callback failed") {
		t.Fatalf("stderr fallback missing callback panic: %q", stderr.String())
	}
}

func TestRotationDefaultsAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "app.log")
	defaultPath := filepath.Join(dir, "default.log")
	defaultBackend := testBackend(t, LogConfig{LogFile: defaultPath, MaxLogFiles: 1})
	if got := defaultBackend.rotator.(*secureRotator).threshold; got != 1024*1024 {
		t.Fatalf("default threshold = %d, want %d", got, 1024*1024)
	}
	b := testBackend(t, LogConfig{LogFile: path, MaxLogFiles: 1, MaxLogFileSizeKB: 1})
	b.Logger("TEST").Info(strings.Repeat("x", 1200))
	b.Logger("TEST").Info(strings.Repeat("y", 1200))
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0700)
	assertMode(t, path, 0600)
	archives, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("got archives %v", archives)
	}
	assertMode(t, archives[0], 0600)
}

func TestConfigurationAndZeroBuffer(t *testing.T) {
	if _, err := NewLogBackend(LogConfig{LogFile: "x.log"}); err == nil {
		t.Fatal("expected zero archive count error")
	}
	if _, err := NewLogBackend(LogConfig{MaxBufferLines: -1}); err == nil {
		t.Fatal("expected negative buffer error")
	}
	b := testBackend(t, LogConfig{})
	b.Logger("TEST").Info("not retained")
	if got := b.LastLogLines(1); len(got) != 0 {
		t.Fatalf("zero buffer retained %v", got)
	}

	b = testBackend(t, LogConfig{MaxBufferLines: 1})
	b.Logger("TEST").Info("copy")
	lines := b.LastLogLines(1)
	lines[0] = "mutated"
	if got := b.LastLogLines(1)[0]; got == "mutated" {
		t.Fatal("recent logs returned mutable internal storage")
	}
}

func TestConcurrentWritesReadsAndClose(t *testing.T) {
	b := testBackend(t, LogConfig{MaxBufferLines: 64})
	var callbacks atomic.Int64
	b.recordCb = func(LogRecord) { callbacks.Add(1) }
	var writers sync.WaitGroup
	for i := 0; i < 16; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			logger := b.Logger(fmt.Sprintf("S%d", i))
			for j := 0; j < 100; j++ {
				logger.Infof("message %d", j)
			}
		}(i)
	}
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				_ = b.LastLogLines(32)
			}
		}()
	}
	writers.Wait()
	readers.Wait()
	if callbacks.Load() != 1600 {
		t.Fatalf("got %d callbacks", callbacks.Load())
	}
	if got := len(b.LastLogLines(1000)); got != 64 {
		t.Fatalf("buffer length = %d", got)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b.Logger("late").Info("ignored")
	if callbacks.Load() != 1600 {
		t.Fatal("callback invoked after close")
	}
}

func TestCloseWaitsForAcceptedCallbackAndWritesRace(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	b := testBackend(t, LogConfig{RecordCallback: func(LogRecord) {
		close(started)
		<-release
	}})
	writerDone := make(chan struct{})
	go func() {
		b.Logger("TEST").Info("accepted")
		close(writerDone)
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- b.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close returned before callback completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-writerDone

	b = testBackend(t, LogConfig{})
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				b.Logger("RACE").Info("write")
			}
		}()
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	writers.Wait()
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type memorySink struct {
	mu sync.Mutex
	bytes.Buffer
}

func (s *memorySink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Buffer.Write(p)
}
func (s *memorySink) Close() error   { return nil }
func (s *memorySink) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.Buffer.String() }

type failingSink struct{ writeErr, closeErr error }

func (s failingSink) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}
func (s failingSink) Close() error { return s.closeErr }

var _ io.Writer = failingWriter{}
