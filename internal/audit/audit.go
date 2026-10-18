package audit

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event string

const (
	EventToolCall     Event = "tool_call"
	EventPolicyDenied Event = "policy_denied"
	EventAuthFailed   Event = "auth_failed"
)

type Entry struct {
	TS         string `json:"ts"`
	Event      Event  `json:"event"`
	Principal  string `json:"principal,omitempty"`
	Upstream   string `json:"upstream,omitempty"`
	Tool       string `json:"tool,omitempty"`
	OK         *bool  `json:"ok,omitempty"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Logger struct {
	mu   sync.Mutex
	file *os.File
}

func New(path string) (*Logger, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(absolute, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open audit log %s: %w", absolute, err)
	}
	return &Logger{file: file}, nil
}

func (l *Logger) Log(entry Entry) {
	entry.TS = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[audit] marshal failed: %v", err)
		return
	}

	l.mu.Lock()
	_, writeErr := l.file.Write(append(line, '\n'))
	l.mu.Unlock()
	if writeErr != nil {
		log.Printf("[audit] write failed: %v", writeErr)
	}

	outcome := string(entry.Event)
	if entry.Event == EventToolCall {
		if entry.OK != nil && *entry.OK {
			outcome = "ok"
		} else {
			outcome = "error"
		}
	}
	principal := entry.Principal
	if principal == "" {
		principal = "-"
	}
	tool := entry.Tool
	if tool == "" {
		tool = "-"
	}
	suffix := ""
	if entry.DurationMs != nil {
		suffix = fmt.Sprintf(" %dms", *entry.DurationMs)
	}
	log.Printf("[audit] %s principal=%s tool=%s %s%s", entry.Event, principal, tool, outcome, suffix)
}

func (l *Logger) Close() error {
	return l.file.Close()
}

func Bool(v bool) *bool    { return &v }
func Int64(v int64) *int64 { return &v }
