package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// Log is the local append-only alert log on the central machine
// (FR-SEC31): one JSON object per line, an "alert" entry before delivery
// is tried and a "delivery" entry after. A retried delivery may write the
// same occurrence twice; readers group entries by occurrence.
//
// The file is opened for appending only, never through a symlink, and
// each line is one write followed by an fsync, so concurrent processes
// (the worker and the CLI) interleave whole lines. Making the file
// append-only for its owner too is the operating system's job: the
// runbook gives the chflags and chattr commands.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// ErrLogNotRegular means the alert log path is not a regular file.
var ErrLogNotRegular = errors.New("alert log is not a regular file")

// OpenLog opens or creates the log at path with mode 0600. Its directory
// must exist.
func OpenLog(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening alert log %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("checking alert log %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrLogNotRegular, path)
	}
	return &Log{f: f, path: path}, nil
}

// Close closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// entry is one line of the log.
type entry struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	// Set on an alert entry.
	Alert *Alert `json:"alert,omitempty"`
	// Set on a delivery entry.
	Occurrence string `json:"occurrence,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Issue      int    `json:"issue,omitempty"`
	URL        string `json:"url,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (l *Log) appendAlert(a Alert) error {
	return l.write(entry{Type: "alert", At: a.At, Alert: &a})
}

func (l *Log) appendDelivery(occurrence string, at time.Time, d Delivery, derr error) error {
	e := entry{Type: "delivery", At: at, Occurrence: occurrence, Outcome: "delivered", Issue: d.Issue, URL: d.URL}
	if derr != nil {
		// GitHub errors carry no secrets (github.APIError), but the text
		// is redacted again here: this file outlives every token.
		e.Outcome, e.Issue, e.URL, e.Error = "failed", 0, "", redactError(derr)
	}
	return l.write(e)
}

func (l *Log) write(e entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding alert log entry: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("writing alert log %s: %w", l.path, err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("syncing alert log %s: %w", l.path, err)
	}
	return nil
}
