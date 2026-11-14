// Package logx provides ButterCams logging: slog with JSON output, daily
// rotating files, retention by age and total size, and redaction of
// credentials (spec 10.3).
package logx

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jxburros/buttercams/internal/config"
)

// MaxTotalBytes caps the size of the logs folder.
const MaxTotalBytes int64 = 2 << 30

// maxFileBytes rolls a single day's file before it can exceed the total cap.
const maxFileBytes int64 = 256 << 20

const filePrefix = "buttercams-"

// Files is an io.Writer that appends to <dir>/buttercams-YYYY-MM-DD.log and
// starts a new file when the local date changes.
type Files struct {
	mu            sync.Mutex
	dir           string
	retentionDays int
	maxTotal      int64
	now           func() time.Time

	f    *os.File
	day  string
	size int64
	part int
	tee  io.Writer
}

// OpenFiles creates the log folder and returns a rotating writer. tee, when
// not nil, receives a copy of every line (console mode).
func OpenFiles(dir string, retentionDays int, tee io.Writer) (*Files, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fs := &Files{dir: dir, retentionDays: retentionDays, maxTotal: MaxTotalBytes, now: time.Now, tee: tee}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.rotateLocked(); err != nil {
		return nil, err
	}
	fs.pruneLocked()
	return fs, nil
}

// SetRetentionDays changes how long log files are kept.
func (fs *Files) SetRetentionDays(days int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.retentionDays = days
	fs.pruneLocked()
}

func (fs *Files) fileName() string {
	if fs.part == 0 {
		return filePrefix + fs.day + ".log"
	}
	return fmt.Sprintf("%s%s.%d.log", filePrefix, fs.day, fs.part)
}

func (fs *Files) rotateLocked() error {
	day := fs.now().Format("2006-01-02")
	if fs.f != nil && day == fs.day && fs.size < maxFileBytes {
		return nil
	}
	if fs.f != nil {
		fs.f.Close()
		fs.f = nil
	}
	if day != fs.day {
		fs.day, fs.part = day, 0
	} else {
		fs.part++
	}
	for {
		path := filepath.Join(fs.dir, fs.fileName())
		st, err := os.Stat(path)
		if err == nil && st.Size() >= maxFileBytes {
			fs.part++
			continue
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		fs.f = f
		fs.size = 0
		if st != nil {
			fs.size = st.Size()
		}
		return nil
	}
}

// Write appends p, redacting credentials. Logging never fails the caller.
func (fs *Files) Write(p []byte) (int, error) {
	clean := []byte(config.Redact(string(p)))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	prevDay := fs.day
	if err := fs.rotateLocked(); err == nil && fs.f != nil {
		n, _ := fs.f.Write(clean)
		fs.size += int64(n)
	}
	if fs.day != prevDay {
		fs.pruneLocked()
	}
	if fs.tee != nil {
		fs.tee.Write(clean)
	}
	return len(p), nil
}

// WriteLine appends one already-formatted line (from a child's stderr).
func (fs *Files) WriteLine(line []byte) {
	if !bytes.HasSuffix(line, []byte("\n")) {
		line = append(append([]byte{}, line...), '\n')
	}
	fs.Write(line)
}

// Prune removes log files that are too old or exceed the total size cap.
func (fs *Files) Prune() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.pruneLocked()
}

func (fs *Files) pruneLocked() {
	entries, err := os.ReadDir(fs.dir)
	if err != nil {
		return
	}
	type lf struct {
		path string
		mod  time.Time
		size int64
	}
	var files []lf
	var total int64
	current := ""
	if fs.f != nil {
		current = fs.f.Name()
	}
	cutoff := fs.now().AddDate(0, 0, -fs.retentionDays)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".log") {
			continue // only files ButterCams wrote
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(fs.dir, name)
		if path != current {
			day := strings.TrimPrefix(name, filePrefix)
			if len(day) >= 10 {
				if d, err := time.ParseInLocation("2006-01-02", day[:10], fs.now().Location()); err == nil &&
					d.AddDate(0, 0, 1).Before(cutoff) {
					os.Remove(path)
					continue
				}
			}
		}
		files = append(files, lf{path, info.ModTime(), info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= fs.maxTotal {
			break
		}
		if f.path == current {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// Close closes the current file.
func (fs *Files) Close() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.f != nil {
		fs.f.Close()
		fs.f = nil
	}
}

// redactWriter redacts credentials on their way to an io.Writer.
type redactWriter struct{ w io.Writer }

func (r redactWriter) Write(p []byte) (int, error) {
	if _, err := r.w.Write([]byte(config.Redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// New returns a JSON slog logger writing to w, tagged with the process name.
func New(w io.Writer, proc string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("proc", proc)
}

// NewChild returns the logger a supervised child uses: JSON lines on stderr,
// which the guardian captures into its log file.
func NewChild(proc string) *slog.Logger {
	return New(redactWriter{os.Stderr}, proc)
}
