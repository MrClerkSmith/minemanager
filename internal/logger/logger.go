// Package logger is a broadcast console logger. Every line written to a
// Minecraft server console is persisted to a file, kept in a bounded in-memory
// ring buffer for the Web UI history, and fanned out to live SSE subscribers.
package logger

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level is a coarse console severity derived from the log line contents.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Line is one console line as exposed over the API and SSE stream.
type Line struct {
	Time  time.Time `json:"time"`
	Text  string    `json:"text"`
	Level Level     `json:"level"`
}

const (
	historySize   = 1000
	subscriberBuf = 512
)

// Logger fans out console output to disk, a ring buffer and subscribers.
type Logger struct {
	mu     sync.RWMutex
	file   *os.File
	ring   []Line
	subs   map[chan Line]struct{}
	closed bool
}

// New creates a logger appending to logs/latest.log inside dir.
func New(dir string) (*Logger, error) {
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(logDir, "latest.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{
		file: f,
		subs: make(map[chan Line]struct{}),
	}, nil
}

// Classify derives a severity from typical Minecraft log markers.
func Classify(text string) Level {
	switch {
	case strings.Contains(text, "ERROR"),
		strings.Contains(text, "FATAL"),
		strings.Contains(text, "Exception"),
		strings.Contains(text, "crash"),
		strings.Contains(text, "Crash"),
		strings.Contains(text, "SEVERE"),
		strings.Contains(text, "failed"),
		strings.Contains(text, "Failed"):
		return LevelError
	case strings.Contains(text, "WARN"), strings.Contains(text, "Warning"):
		return LevelWarn
	}
	return LevelInfo
}

// Write appends one raw line (no trailing newline required). Use this for
// console output that may contain literal '%' characters.
func (l *Logger) Write(text string) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	text = strings.TrimRight(text, "\r\n")
	line := Line{
		Time:  time.Now(),
		Text:  text,
		Level: Classify(text),
	}
	if l.file != nil {
		fmt.Fprintf(l.file, "[%s] [%s] %s\n",
			line.Time.Format("2006-01-02 15:04:05"), line.Level, text)
	}
	l.ring = append(l.ring, line)
	if len(l.ring) > historySize {
		l.ring = l.ring[len(l.ring)-historySize:]
	}
	subs := make([]chan Line, 0, len(l.subs))
	for ch := range l.subs {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- line:
		default: // slow subscriber: drop line rather than block the server
		}
	}
}

// Writef appends one formatted line.
func (l *Logger) Writef(format string, args ...any) {
	l.Write(fmt.Sprintf(format, args...))
}

// History returns up to the last n lines (n<=0 => everything buffered).
func (l *Logger) History(n int) []Line {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > len(l.ring) {
		n = len(l.ring)
	}
	out := make([]Line, n)
	copy(out, l.ring[len(l.ring)-n:])
	return out
}

// Subscribe returns a channel receiving future lines plus the current history.
// The returned channel must be released with Unsubscribe.
func (l *Logger) Subscribe() (chan Line, []Line) {
	ch := make(chan Line, subscriberBuf)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		close(ch)
		return ch, nil
	}
	l.subs[ch] = struct{}{}
	// Copy the ring under the same lock: History() re-enters the mutex and
	// would self-deadlock.
	hist := make([]Line, len(l.ring))
	copy(hist, l.ring)
	return ch, hist
}

// Unsubscribe removes and closes a subscriber channel.
func (l *Logger) Unsubscribe(ch chan Line) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.subs[ch]; ok {
		delete(l.subs, ch)
		close(ch)
	}
}

// Close stops accepting lines and flushes subscribers.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for ch := range l.subs {
		close(ch)
		delete(l.subs, ch)
	}
	if l.file != nil {
		_ = l.file.Close()
	}
}

// Scanner reads a stream line by line and forwards it to the logger.
// Long lines (Java stack traces can be single-line) are split into chunks so
// that the ring buffer and SSE stream stay bounded.
func (l *Logger) Scanner(r io.Reader, chunk int) {
	scanner := bufio.NewScanner(r)
	if chunk > 0 {
		scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
	}
	for scanner.Scan() {
		text := scanner.Text()
		for chunk > 0 && len(text) > chunk {
			l.Write(text[:chunk])
			text = text[chunk:]
		}
		l.Write(text)
	}
}
