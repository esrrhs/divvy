package agent

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cCyan   = "\033[36m"
	cBlue   = "\033[34m"
)

// ansiRE strips terminal color codes when mirroring output to the session log.
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Logger is a small colorized CLI logger. When Tee has been given a file,
// every line is additionally persisted there as plain text with a timestamp
// and level, so a finished run can still be inspected. All methods are safe
// for concurrent use (parallel leaf workers share one logger).
type Logger struct {
	mu      sync.Mutex
	out     io.Writer
	err     io.Writer
	color   bool
	verbose bool
	file    io.Writer

	wMu   sync.Mutex
	watch map[int]chan string
	wID   int
}

// logLineBuffer bounds how many rendered lines a slow UI subscriber may queue.
const logLineBuffer = 256

// SubscribeLines fans out every rendered (plain, timestamped) log line. It is
// the live source for the web UI log panel. Delivery is best-effort: a slow
// subscriber drops lines instead of blocking workers.
func (l *Logger) SubscribeLines() (<-chan string, func()) {
	if l == nil {
		ch := make(chan string)
		return ch, func() { close(ch) }
	}
	l.wMu.Lock()
	l.wID++
	id := l.wID
	if l.watch == nil {
		l.watch = make(map[int]chan string)
	}
	ch := make(chan string, logLineBuffer)
	l.watch[id] = ch
	l.wMu.Unlock()
	cancel := func() {
		l.wMu.Lock()
		if c, ok := l.watch[id]; ok {
			delete(l.watch, id)
			close(c)
		}
		l.wMu.Unlock()
	}
	return ch, cancel
}

// fanLines delivers one plain line to every watcher without blocking.
func (l *Logger) fanLines(line string) {
	l.wMu.Lock()
	defer l.wMu.Unlock()
	for _, ch := range l.watch {
		select {
		case ch <- line:
		default:
		}
	}
}

// NewLogger writes to stdout/stderr.
func NewLogger(verbose bool) *Logger {
	color := os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)
	return &Logger{out: os.Stdout, err: os.Stderr, color: color, verbose: verbose}
}

// Tee additionally persists all subsequent output to w (e.g. the session log
// file). Screen styling is removed and each line gains a timestamp and level.
func (l *Logger) Tee(w io.Writer) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.file = w
	l.mu.Unlock()
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (l *Logger) paint(code, s string) string {
	if l == nil || !l.color {
		return s
	}
	return code + s + cReset
}

// emit prints one prefixed line to the screen and mirrors a timestamped,
// de-colored "LEVEL message" line to the session log file.
func (l *Logger) emit(w io.Writer, colorCode, icon, level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	screen := l.paint(colorCode, icon) + msg
	plain := ansiRE.ReplaceAllString(msg, "")

	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(w, screen)
	line := fmt.Sprintf("%s %-7s %s",
		time.Now().Format("2006-01-02 15:04:05.000"), level, plain)
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	}
	l.fanLines(line)
}

func (l *Logger) Infof(format string, args ...any) {
	l.emit(l.out, cCyan, "▸ ", "INFO", format, args...)
}

func (l *Logger) Actionf(format string, args ...any) {
	l.emit(l.out, cBlue, "  → ", "ACTION", format, args...)
}

func (l *Logger) Okf(format string, args ...any) {
	l.emit(l.out, cGreen, "  ✓ ", "OK", format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.emit(l.out, cYellow, "  ! ", "WARN", format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.emit(l.err, cRed, "  ✗ ", "ERROR", format, args...)
}

func (l *Logger) Debugf(format string, args ...any) {
	if l == nil || !l.verbose {
		return
	}
	l.emit(l.out, cDim, "  · ", "DEBUG", format, args...)
}

func (l *Logger) Banner(s string) {
	line := strings.Repeat("─", 56)
	l.Print(l.paint(cDim, line) + "\n" + l.paint(cBold, s))
}

// Print writes free-form text (e.g. rendered trees) verbatim to the screen
// and, de-colored, to the session log.
func (l *Logger) Print(s string) {
	plain := ansiRE.ReplaceAllString(s, "")
	if l == nil {
		fmt.Fprint(os.Stdout, s)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(l.out, s)
	if !strings.HasSuffix(s, "\n") {
		fmt.Fprintln(l.out)
	}
	if l.file != nil {
		fmt.Fprint(l.file, plain)
		if !strings.HasSuffix(plain, "\n") {
			fmt.Fprintln(l.file)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(plain, "\n"), "\n") {
		l.fanLines(line)
	}
}

// SilentLogger discards all output (tests).
func SilentLogger() *Logger {
	return &Logger{out: io.Discard, err: io.Discard, color: false, verbose: false}
}
