package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/decred/slog"
	"github.com/vctt94/bisonbotkit/utils"
)

const (
	defaultMaxLogFileSizeKB = 1024
	maxBufferedLineBytes    = 1 << 20
)

// errMsgRE is a regexp that matches error log msgs.
var errMsgRE = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} \[ERR] `)

var errBackendClosed = errors.New("logging backend is closed")

// LogRecord describes one record emitted by a subsystem logger.
type LogRecord struct {
	Timestamp time.Time
	Subsystem string
	Level     slog.Level
	Message   string
	Formatted string
}

// LogBuffer is a simple bounded buffer of recent log lines. A zero capacity
// disables buffering.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewLogBuffer creates a new buffer with the specified max size. Non-positive
// sizes disable buffering.
func NewLogBuffer(maxLines int) *LogBuffer {
	if maxLines < 0 {
		maxLines = 0
	}
	return &LogBuffer{max: maxLines}
}

// Write adds a log line to the buffer.
func (b *LogBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.max == 0 {
		return n, nil
	}

	// Do not let a single malformed record retain an unbounded allocation.
	if len(p) > maxBufferedLineBytes {
		p = p[:maxBufferedLineBytes]
	}
	line := string(p)

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == b.max {
		copy(b.lines, b.lines[1:])
		b.lines[len(b.lines)-1] = line
		return n, nil
	}
	b.lines = append(b.lines, line)
	return n, nil
}

// LastLogLines returns a copy of the n most recent log lines.
func (b *LogBuffer) LastLogLines(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || len(b.lines) == 0 {
		return []string{}
	}
	if n > len(b.lines) {
		n = len(b.lines)
	}
	result := make([]string, n)
	copy(result, b.lines[len(b.lines)-n:])
	return result
}

// LogConfig contains configuration options for the logging system.
type LogConfig struct {
	LogFile              string // Path to log file (empty for no log file)
	DebugLevel           string // Debug level string in format "subsys=level,subsys2=level2"
	MaxLogFileSizeKB     int    // Rotation threshold; zero defaults to 1024 KB
	MaxLogFiles          int    // Number of archived log files to keep; must be positive with LogFile
	MaxBufferLines       int    // Maximum number of log lines to buffer; zero disables buffering
	LogCallback          func(string)
	RecordCallback       func(LogRecord)
	ErrorCallback        func(string) // Legacy callback for formatted [ERR] messages
	BackendErrorCallback func(error)  // Runtime sink and callback errors
	UseStdout            *bool        // Whether to output logs to stdout (defaults to true)
}

type logItem struct {
	record LogRecord
}

// LogBackend owns subsystem loggers and serializes output to all configured
// destinations. It is safe for concurrent use.
type LogBackend struct {
	rotator fileSink
	stdout  io.Writer
	stderr  io.Writer

	defaultLogLevel slog.Level
	logLevels       map[string]slog.Level
	loggersMtx      sync.Mutex
	loggers         map[string]*subsystemLogger

	logCb        func(string)
	recordCb     func(LogRecord)
	errorMsg     func(string)
	backendError func(error)
	logBuffer    *LogBuffer
	useStdout    bool
	flags        uint32

	queueMtx    sync.Mutex
	queueCond   *sync.Cond
	pending     []logItem
	dispatching bool
	closing     bool
	closed      bool
	closeErr    error
}

const (
	flagLongFile uint32 = 1 << iota
	flagShortFile
	flagUTC
	flagNoDateTime
)

// NewLogBackend creates a new logging backend.
func NewLogBackend(config LogConfig) (*LogBackend, error) {
	if config.MaxLogFileSizeKB < 0 {
		return nil, fmt.Errorf("max log file size must not be negative")
	}
	if config.MaxBufferLines < 0 {
		return nil, fmt.Errorf("max buffer lines must not be negative")
	}
	if config.LogFile != "" && config.MaxLogFiles <= 0 {
		return nil, fmt.Errorf("max log files must be positive when a log file is configured")
	}

	maxFileSizeKB := config.MaxLogFileSizeKB
	if maxFileSizeKB == 0 {
		maxFileSizeKB = defaultMaxLogFileSizeKB
	}

	useStdout := true
	if config.UseStdout != nil {
		useStdout = *config.UseStdout
	}

	b := &LogBackend{
		defaultLogLevel: slog.LevelInfo,
		logLevels:       make(map[string]slog.Level),
		loggers:         make(map[string]*subsystemLogger),
		logBuffer:       NewLogBuffer(config.MaxBufferLines),
		logCb:           config.LogCallback,
		recordCb:        config.RecordCallback,
		errorMsg:        config.ErrorCallback,
		backendError:    config.BackendErrorCallback,
		useStdout:       useStdout,
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		flags:           logFlagsFromEnvironment(),
	}
	b.queueCond = sync.NewCond(&b.queueMtx)

	if config.LogFile != "" {
		logFile := utils.CleanAndExpandPath(config.LogFile)
		r, err := newSecureRotator(logFile, int64(maxFileSizeKB)*1024, config.MaxLogFiles)
		if err != nil {
			return nil, fmt.Errorf("failed to create file rotator: %w", err)
		}
		b.rotator = r
	}

	if err := b.applyDebugLevels(config.DebugLevel); err != nil {
		if b.rotator != nil {
			_ = b.rotator.Close()
		}
		return nil, err
	}
	return b, nil
}

func (b *LogBackend) applyDebugLevels(debugLevel string) error {
	if debugLevel == "" {
		return nil
	}
	for _, v := range strings.Split(debugLevel, ",") {
		fields := strings.Split(v, "=")
		switch len(fields) {
		case 1:
			if fields[0] == "" {
				continue
			}
			level, ok := slog.LevelFromString(fields[0])
			if !ok {
				return fmt.Errorf("unknown log level %q", fields[0])
			}
			b.defaultLogLevel = level
		case 2:
			level, ok := slog.LevelFromString(fields[1])
			if !ok {
				return fmt.Errorf("unknown log level %q", fields[1])
			}
			b.logLevels[fields[0]] = level
		default:
			return fmt.Errorf("unable to parse %q as subsys=level debuglevel string", v)
		}
	}
	return nil
}

// Write implements io.Writer for compatibility with callers that provide
// already-formatted records. Such records have no subsystem metadata.
func (b *LogBackend) Write(p []byte) (int, error) {
	formatted := string(p)
	err := b.enqueue(LogRecord{
		Timestamp: time.Now(),
		Level:     slog.LevelInfo,
		Message:   strings.TrimSuffix(formatted, "\n"),
		Formatted: formatted,
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (b *LogBackend) enqueue(record LogRecord) error {
	b.queueMtx.Lock()
	if b.closing || b.closed {
		b.queueMtx.Unlock()
		return errBackendClosed
	}
	b.pending = append(b.pending, logItem{record: record})
	if b.dispatching {
		b.queueMtx.Unlock()
		return nil
	}
	b.dispatching = true
	b.queueMtx.Unlock()

	for {
		b.queueMtx.Lock()
		if len(b.pending) == 0 {
			b.dispatching = false
			b.queueCond.Broadcast()
			b.queueMtx.Unlock()
			return nil
		}
		item := b.pending[0]
		b.pending = b.pending[1:]
		b.queueMtx.Unlock()
		b.process(item.record)
	}
}

func (b *LogBackend) process(record LogRecord) {
	var errs []error
	if b.rotator != nil {
		if err := writeRecord(b.rotator, record.Formatted); err != nil {
			errs = append(errs, fmt.Errorf("write log file: %w", err))
		}
	}
	if b.useStdout {
		if err := writeRecord(b.stdout, record.Formatted); err != nil {
			errs = append(errs, fmt.Errorf("write stdout: %w", err))
		}
	}
	if _, err := b.logBuffer.Write([]byte(record.Formatted)); err != nil {
		errs = append(errs, fmt.Errorf("buffer log record: %w", err))
	}

	if b.logCb != nil {
		if err := callSafely(func() { b.logCb(record.Formatted) }); err != nil {
			errs = append(errs, fmt.Errorf("log callback: %w", err))
		}
	}
	if b.recordCb != nil {
		if err := callSafely(func() { b.recordCb(record) }); err != nil {
			errs = append(errs, fmt.Errorf("record callback: %w", err))
		}
	}
	if b.errorMsg != nil && errMsgRE.MatchString(record.Formatted) {
		line := record.Formatted[24:]
		if err := callSafely(func() { b.errorMsg(line) }); err != nil {
			errs = append(errs, fmt.Errorf("error callback: %w", err))
		}
	}
	for _, err := range errs {
		b.reportError(err)
	}
}

func writeRecord(w io.Writer, formatted string) error {
	p := []byte(formatted)
	n, err := w.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func callSafely(f func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	f()
	return nil
}

func (b *LogBackend) reportError(err error) {
	if b.backendError != nil {
		if callbackErr := callSafely(func() { b.backendError(err) }); callbackErr == nil {
			return
		} else {
			_, _ = fmt.Fprintf(b.stderr, "logging error callback failed: %v\n", callbackErr)
		}
	}
	_, _ = fmt.Fprintf(b.stderr, "logging backend: %v\n", err)
}

// Logger returns a logger for the given subsystem.
func (b *LogBackend) Logger(subsys string) slog.Logger {
	b.loggersMtx.Lock()
	defer b.loggersMtx.Unlock()
	if l, ok := b.loggers[subsys]; ok {
		return l
	}
	level := b.defaultLogLevel
	if configured, ok := b.logLevels[subsys]; ok {
		level = configured
	}
	l := &subsystemLogger{backend: b, subsystem: subsys}
	l.SetLevel(level)
	b.loggers[subsys] = l
	return l
}

// SetLogLevel changes the logging level for a specific subsystem or the default.
func (b *LogBackend) SetLogLevel(s string) error {
	if s == "" {
		return nil
	}
	fields := strings.Split(s, "=")
	b.loggersMtx.Lock()
	defer b.loggersMtx.Unlock()
	switch len(fields) {
	case 1:
		level, ok := slog.LevelFromString(fields[0])
		if !ok {
			return fmt.Errorf("unknown log level %q", fields[0])
		}
		b.defaultLogLevel = level
		for subsys, logger := range b.loggers {
			if _, hasSpecific := b.logLevels[subsys]; !hasSpecific {
				logger.SetLevel(level)
			}
		}
	case 2:
		level, ok := slog.LevelFromString(fields[1])
		if !ok {
			return fmt.Errorf("unknown log level %q", fields[1])
		}
		b.logLevels[fields[0]] = level
		if logger, ok := b.loggers[fields[0]]; ok {
			logger.SetLevel(level)
		}
	default:
		return fmt.Errorf("unable to parse %q as subsys=level debuglevel string", s)
	}
	return nil
}

// LastLogLines returns the n most recent log lines.
func (b *LogBackend) LastLogLines(n int) []string { return b.logBuffer.LastLogLines(n) }

// Close shuts down the logger and waits for all accepted records and callbacks.
func (b *LogBackend) Close() error {
	b.queueMtx.Lock()
	if b.closed {
		err := b.closeErr
		b.queueMtx.Unlock()
		return err
	}
	if b.closing {
		for !b.closed {
			b.queueCond.Wait()
		}
		err := b.closeErr
		b.queueMtx.Unlock()
		return err
	}
	b.closing = true
	for b.dispatching {
		b.queueCond.Wait()
	}
	b.queueMtx.Unlock()

	var closeErr error
	if b.rotator != nil {
		closeErr = b.rotator.Close()
		if closeErr != nil {
			b.reportError(fmt.Errorf("close log file: %w", closeErr))
		}
	}
	b.queueMtx.Lock()
	b.closeErr = closeErr
	b.closed = true
	b.queueCond.Broadcast()
	b.queueMtx.Unlock()
	return closeErr
}

type subsystemLogger struct {
	backend   *LogBackend
	subsystem string
	level     atomic.Uint32
}

func (l *subsystemLogger) Level() slog.Level         { return slog.Level(l.level.Load()) }
func (l *subsystemLogger) SetLevel(level slog.Level) { l.level.Store(uint32(level)) }

func (l *subsystemLogger) Trace(v ...interface{})            { l.print(slog.LevelTrace, v...) }
func (l *subsystemLogger) Debug(v ...interface{})            { l.print(slog.LevelDebug, v...) }
func (l *subsystemLogger) Info(v ...interface{})             { l.print(slog.LevelInfo, v...) }
func (l *subsystemLogger) Warn(v ...interface{})             { l.print(slog.LevelWarn, v...) }
func (l *subsystemLogger) Error(v ...interface{})            { l.print(slog.LevelError, v...) }
func (l *subsystemLogger) Critical(v ...interface{})         { l.print(slog.LevelCritical, v...) }
func (l *subsystemLogger) Tracef(f string, v ...interface{}) { l.printf(slog.LevelTrace, f, v...) }
func (l *subsystemLogger) Debugf(f string, v ...interface{}) { l.printf(slog.LevelDebug, f, v...) }
func (l *subsystemLogger) Infof(f string, v ...interface{})  { l.printf(slog.LevelInfo, f, v...) }
func (l *subsystemLogger) Warnf(f string, v ...interface{})  { l.printf(slog.LevelWarn, f, v...) }
func (l *subsystemLogger) Errorf(f string, v ...interface{}) { l.printf(slog.LevelError, f, v...) }
func (l *subsystemLogger) Criticalf(f string, v ...interface{}) {
	l.printf(slog.LevelCritical, f, v...)
}

func (l *subsystemLogger) print(level slog.Level, args ...interface{}) {
	if l.Level() <= level {
		l.emit(level, strings.TrimSuffix(fmt.Sprintln(args...), "\n"))
	}
}
func (l *subsystemLogger) printf(level slog.Level, format string, args ...interface{}) {
	if l.Level() <= level {
		l.emit(level, fmt.Sprintf(format, args...))
	}
}
func (l *subsystemLogger) emit(level slog.Level, message string) {
	now := time.Now()
	if l.backend.flags&flagUTC != 0 {
		now = now.UTC()
	}
	record := LogRecord{Timestamp: now, Subsystem: l.subsystem, Level: level, Message: message}
	record.Formatted = l.backend.format(record)
	_ = l.backend.enqueue(record)
}

func (b *LogBackend) format(record LogRecord) string {
	var header string
	if b.flags&flagNoDateTime == 0 {
		header = record.Timestamp.Format("2006-01-02 15:04:05.000 ")
	}
	header += "[" + record.Level.String() + "] " + record.Subsystem
	if b.flags&(flagLongFile|flagShortFile) != 0 {
		_, file, line, ok := runtime.Caller(4)
		if !ok {
			file = "???"
			line = 0
		}
		if b.flags&flagShortFile != 0 {
			if i := strings.LastIndexAny(file, "/\\"); i >= 0 {
				file = file[i+1:]
			}
		}
		header += fmt.Sprintf(" %s:%d", file, line)
	}
	return header + ": " + record.Message + "\n"
}

func logFlagsFromEnvironment() uint32 {
	var flags uint32
	for _, value := range strings.Split(os.Getenv("LOGFLAGS"), ",") {
		switch value {
		case "longfile":
			flags |= flagLongFile
		case "shortfile":
			flags |= flagShortFile
		case "UTC":
			flags |= flagUTC
		case "nodatetime":
			flags |= flagNoDateTime
		}
	}
	return flags
}
