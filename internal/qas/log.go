package qas

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	RunID     string `json:"run_id,omitempty"`
	Cursor    int64  `json:"cursor"`
}

type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	cursor  int64
	limit   int
}

var (
	bearerLogRE    = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	sensitiveLogRE = regexp.MustCompile(`(?i)(["']?(authorization|api[_-]?key|password|passwd|token|stoken|access[_-]?token|refresh[_-]?token|cookie|secret|push[_-]?key|deer[_-]?key|qywx[_-]?key|bark[_-]?push|ntfy[_-]?(url|topic)|webhook[_-]?url|smtp[_-]?(password|pass))\b["']?\s*[:=]\s*["']?)([^"'\s,;}]+)`)
)

func NewLogBuffer(limit int) *LogBuffer {
	if limit < 1 {
		limit = 2000
	}
	return &LogBuffer{limit: limit}
}

func redactText(value any) string {
	text := fmt.Sprint(value)
	text = bearerLogRE.ReplaceAllString(text, "Bearer [REDACTED]")
	return sensitiveLogRE.ReplaceAllString(text, "$1[REDACTED]")
}

func (b *LogBuffer) Add(level, runID, format string, args ...any) string {
	message := redactText(fmt.Sprintf(format, args...))
	log.Printf("[%s] %s", strings.ToUpper(level), message)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cursor++
	b.entries = append(b.entries, LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     strings.ToUpper(level),
		Message:   message,
		RunID:     runID,
		Cursor:    b.cursor,
	})
	if len(b.entries) > b.limit {
		b.entries = append([]LogEntry(nil), b.entries[len(b.entries)-b.limit:]...)
	}
	return message
}

func (b *LogBuffer) Query(query, level, runID string, cursor int64, limit int) map[string]any {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query = strings.ToLower(query)
	level = strings.ToUpper(level)
	b.mu.RLock()
	entries := append([]LogEntry(nil), b.entries...)
	b.mu.RUnlock()
	result := make([]LogEntry, 0, limit)
	all := make([]LogEntry, 0)
	for _, entry := range entries {
		if entry.Cursor <= cursor || (level != "" && entry.Level != level) || (runID != "" && entry.RunID != runID) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(entry.Message), query) {
			continue
		}
		all = append(all, entry)
		if len(result) < limit {
			result = append(result, entry)
		}
	}
	nextCursor := cursor
	if len(result) > 0 {
		nextCursor = result[len(result)-1].Cursor
	}
	return map[string]any{
		"items":       result,
		"next_cursor": nextCursor,
		"has_more":    len(all) > limit,
	}
}
