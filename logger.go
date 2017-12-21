// logger implements a shared Logger to use across the various
// DigitalRebar packages.  It includes in-memory saving of log
// messages from all sources in a ring buffer and the ability to
// specify a callback function that should be called every time a line
// is logged.
//
// It is not intended to be a standalone logging package, but intead
// is designed to be used in conjunction with other local logging
// packages that provide the usual Printf, Fatalf, and Panicf calls.
package logger

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Level is a log level.  It consists of the usual logging levels.
type Level int

const (
	// Trace should be used when you want detailed log messages that map
	// to the actual flow of control through the program.
	Trace Level = iota
	// Debug should be useful for debugging low-level problems that do
	// not necessarily require the level of detail that Trace provides.
	Debug Level = iota
	// Info should be used to emit information messages that do not
	// signal an unusual condition.
	Info Level = iota
	// Warn should be used to signal when something unusual happened
	// that the program was able to handle appropriatly.
	Warn Level = iota
	// Error should be used to signal when something unusal happened that
	// could not be handled gracefully, but that did not result in a
	// condition where we had to shut down.
	Error Level = iota
	// Fatal should be used where we need to shut down the program in order
	// to avoid data corruption, and there is no possibility of handling
	// in a programmatic fashion.
	Fatal Level = iota
	// Panic should be used where we need to abort whatever is happening,
	// and there is a possibility of handling or avoiding the error in a
	// programmatic fashion.
	Panic Level = iota
)

var seq int64

func (l Level) String() string {
	switch l {
	case Trace:
		return "trace"
	case Debug:
		return "debug"
	case Info:
		return " info"
	case Warn:
		return " warn"
	case Error:
		return "error"
	case Fatal:
		return "fatal"
	case Panic:
		return "panic"
	}
	return "unknown"
}

// Line is the smallest unit of things we log.
type Line struct {
	// Group is an abstract number used to group Lines together
	Group int64
	// Seq is the sequence number that the Line was emitted in.
	// Sequence numbers are globally unique.
	Seq int64
	// Time is when the Line was created.
	Time time.Time
	// Service is the name of the log.
	Service string
	// Level is the level at which the line was logged.
	Level Level
	// File is the source file that generated the line
	File string
	// Line is the line number of the line that generated the line.
	Line int
	// Message is the message that was logged.
	Message string
	// Data is any auxillary data that was captured.
	Data []interface{}
}

// Publisher is a function to be called whenever a Line would be added
// to the ring buffer and sent to the local logger.
type Publisher func(l *Line)

func (l *Line) String() string {
	return fmt.Sprintf("[%d:%d]%s [%s]: %s", l.Group, l.Seq, l.Service, l.Level, l.Message)
}

// Local allows us to accept as a local logger anything that has the
// usual basic logging methods
type Local interface {
	Printf(string, ...interface{})
	Fatalf(string, ...interface{})
	Panicf(string, ...interface{})
}

// Logger is the interface that users of this package should expect.
//
// Tracef, Debugf, Infof, Warnf, Errorf, Fatalf, and Panicf are the
// usual logging functions.  They will add Lines to the Buffer based
// on the current Level of the Log.
//
// Level and SetLevel get the current logging Level of the Logger
//
// Service and SetService get and set the Service the Log will create
// Lines as.
//
// Buffer returns a reference to the Buffer that stores messages the
// Log generates.
//
// Fork returns an independent copy of the Log that has its own
// group ID.
//
// With returns a new Logger that shares the same Group as its
// parent, but that has additional data that will be added to any
// Lines the new Logger generates.
type Logger interface {
	Tracef(string, ...interface{})
	Debugf(string, ...interface{})
	Infof(string, ...interface{})
	Warnf(string, ...interface{})
	Errorf(string, ...interface{})
	Fatalf(string, ...interface{})
	Panicf(string, ...interface{})
	Fork() Logger
	With(...interface{}) Logger
	Level() Level
	SetLevel(Level)
	Service() string
	SetService(string)
	Buffer() *Buffer
}

// Buffer is a ringbuffer that can be shared among several different
// Loggers.  Buffers are responsible for flushing messages to local
// loggers and calling Publisher callbacks as needed.
type Buffer struct {
	*sync.Mutex
	nextGroup   int64
	baseLogger  Local
	logs        map[string]*log
	retainLines int
	wrapped     bool
	nextLine    int
	buffer      []*Line
	publisher   Publisher
}

// NewGroup returns a new group number.
func (b *Buffer) NewGroup() int64 {
	atomic.AddInt64(&b.nextGroup, 1)
	return b.nextGroup
}

// SetPublisher sets a callback function to be called whenever
// a Line will be written to a Local logger.
func (b *Buffer) SetPublisher(p Publisher) {
	b.Lock()
	defer b.Unlock()
	b.publisher = p
}

func (b *Buffer) lines(max int) []*Line {
	res := []*Line{}
	if b.retainLines == 0 {
		return res
	}
	if !b.wrapped || b.nextLine == len(b.buffer) {
		end := b.nextLine
		start := 0
		if max > -1 && max < end-start {
			start = end - max
		}
		res = make([]*Line, end-start)
		copy(res, b.buffer[start:end])
	} else {
		end := b.nextLine
		start := b.nextLine
		var firstPart, lastPart []*Line
		if max > -1 {
			if max < end {
				// All the data we want is in 0:b.bufEnd
				start = end - max
				res = make([]*Line, end-start)
				copy(res, b.buffer[start:end])
				return res
			}
			// We need to consume part of the slice from b.bufStart.
			// Figure out how much
			lastPart = b.buffer[:end]
			firstPart = b.buffer[start:]
			max -= len(lastPart)
			if max < len(firstPart) {
				firstPart = firstPart[len(firstPart)-max:]
			}
		} else {
			firstPart = b.buffer[start:]
			lastPart = b.buffer[:end]
		}
		res = make([]*Line, len(firstPart)+len(lastPart))
		copy(res, firstPart)
		copy(res[len(firstPart):], lastPart)
	}
	return res
}

// New returns a new Buffer which will write to the passed-in Local
// logger.  A new Buffer will by default save 1000 Lines in memory.
// This number is adjustable on the fly with the KeepLines method.
func New(base Local) *Buffer {
	return &Buffer{
		Mutex:       &sync.Mutex{},
		baseLogger:  base,
		logs:        map[string]*log{},
		retainLines: 1000,
		buffer:      make([]*Line, 1000),
	}
}

// Log creates or reuses a Log for the passed-in Service.  All logs returned
// for a particular Service by this method will share the same Group
// and have a common Seq.  You can force a log into a different Group
// using that log's Fork method.
func (b *Buffer) Log(service string) Logger {
	b.Lock()
	defer b.Unlock()
	if service == "" {
		service = "default"
	}
	res, ok := b.logs[service]
	if !ok {
		res = &log{
			group:   new(int64),
			base:    b,
			service: service,
			level:   Error,
			aux:     []interface{}{},
		}
		atomic.StoreInt64(res.group, b.NewGroup())
		b.logs[service] = res
	}
	return res
}

// Logs Returns all the Logs directly created by the Log method.  Logs
// created via other means are not tracked by the Buffer.
func (b *Buffer) Logs() []Logger {
	b.Lock()
	defer b.Unlock()
	res := make([]Logger, 0, len(b.logs))
	for _, svc := range b.logs {
		res = append(res, svc)
	}
	return res
}

// Lines returns up to the last count lines logged.  If count is a
// negative number, all the lines we currently have are kept.
func (b *Buffer) Lines(count int) []*Line {
	b.Lock()
	defer b.Unlock()
	return b.lines(count)
}

// MaxLines returns the current number of lines the Logger will keep in memory.
func (b *Buffer) MaxLines() int {
	return b.retainLines
}

// KeepLines sets the max number of lines that will be kept in memory by the Logger.
// It will discard older lines as appropriate.
func (b *Buffer) KeepLines(lines int) {
	b.Lock()
	defer b.Unlock()
	if lines == b.retainLines {
		return
	}
	buffer := b.lines(lines)
	b.buffer = make([]*Line, lines)
	b.wrapped = false
	b.nextLine = len(buffer)
	copy(b.buffer, buffer)
	b.retainLines = lines
}

func (b *Buffer) insertLine(l *Line) {
	if b.retainLines != 0 {
		if b.nextLine == len(b.buffer) {
			if !b.wrapped {
				b.wrapped = true
			}
			b.nextLine = 0
		}
		b.buffer[b.nextLine] = l
		b.nextLine++
	}
	l.Time = time.Now()
	l.Seq = atomic.AddInt64(&seq, 1)
	if b.publisher != nil {
		b.publisher(l)
	}
	if b.baseLogger != nil {
		switch l.Level {
		case Panic:
			b.baseLogger.Panicf("%s [%s]: %s", l.Service, l.Level, l.Message)
		case Fatal:
			b.baseLogger.Fatalf("%s [%s]: %s", l.Service, l.Level, l.Message)
		default:
			b.baseLogger.Printf("%s [%s]: %s", l.Service, l.Level, l.Message)
		}
	}
}

// Log is the default implementation of our Logger interface.
type log struct {
	base    *Buffer
	group   *int64
	service string
	level   Level
	aux     []interface{}
}

func (b *log) addLine(level Level, message string, args ...interface{}) {
	if level < b.level {
		return
	}
	line := &Line{
		Group:   *b.group,
		Level:   level,
		Service: b.service,
		Message: fmt.Sprintf(message, args...),
		Data:    b.aux,
	}
	_, line.File, line.Line, _ = runtime.Caller(2)
	b.base.insertLine(line)
}

func (b *log) Tracef(msg string, args ...interface{}) {
	b.addLine(Trace, msg, args...)
}

func (b *log) Debugf(msg string, args ...interface{}) {
	b.addLine(Debug, msg, args...)
}

func (b *log) Infof(msg string, args ...interface{}) {
	b.addLine(Info, msg, args...)
}

func (b *log) Warnf(msg string, args ...interface{}) {
	b.addLine(Warn, msg, args...)
}

func (b *log) Errorf(msg string, args ...interface{}) {
	b.addLine(Error, msg, args...)
}

func (b *log) Fatalf(msg string, args ...interface{}) {
	b.addLine(Fatal, msg, args...)
}

func (b *log) Panicf(msg string, args ...interface{}) {
	b.addLine(Panic, msg, args...)
}

func (b *log) Buffer() *Buffer {
	return b.base
}

func (b *log) With(args ...interface{}) Logger {
	res := &log{b.base, b.group, b.service, b.level, b.aux}
	res.aux = append(res.aux, args...)
	return res
}

func (b *log) Fork() Logger {
	res := &log{
		base:    b.base,
		service: b.service,
		level:   b.level,
		aux:     []interface{}{},
	}
	grp := b.base.NewGroup()
	res.group = &grp
	return res
}

func (b *log) Level() Level {
	return b.level
}

func (b *log) SetLevel(l Level) {
	b.level = l
}

func (b *log) Service() string {
	return b.service
}

func (b *log) SetService(s string) {
	b.service = s
}