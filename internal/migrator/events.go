package migrator

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// EventLevel classifies migration events.
type EventLevel string

const (
	EventInfo     EventLevel = "info"
	EventWarn     EventLevel = "warn"
	EventError    EventLevel = "error"
	EventProgress EventLevel = "progress"
)

// Event is one log line or progress update emitted by a running migration.
type Event struct {
	Time          time.Time  `json:"time"`
	Level         EventLevel `json:"level"`
	Message       string     `json:"message"`
	Table         string     `json:"table,omitempty"`
	ProcessedRows int64      `json:"processed_rows,omitempty"`
	TotalRows     int64      `json:"total_rows,omitempty"`
}

// EventSink receives migration events. The migrator serializes all sink
// calls behind one mutex, so a sink never runs concurrently with itself even
// when tables migrate in parallel.
type EventSink func(Event)

// WithEventSink routes migration output to sink instead of standard output.
func WithEventSink(sink EventSink) Option {
	return func(migrator *Migrator) error {
		if sink == nil {
			return fmt.Errorf("event sink cannot be nil")
		}
		migrator.sink = sink
		return nil
	}
}

func stdoutSink(event Event) {
	fmt.Println(event.Message)
}

// lockedSink serializes sink calls from concurrently migrating tables.
func lockedSink(sink EventSink) EventSink {
	if sink == nil {
		sink = stdoutSink
	}
	var mu sync.Mutex
	return func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		sink(event)
	}
}

func (m *Migrator) emit(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	event.Message = strings.TrimRight(event.Message, "\n")
	sink := m.sink
	if sink == nil {
		sink = stdoutSink
	}
	sink(event)
}

func (m *Migrator) logf(format string, args ...any) {
	m.emit(Event{Level: EventInfo, Message: fmt.Sprintf(format, args...)})
}

func (m *Migrator) warnf(format string, args ...any) {
	m.emit(Event{Level: EventWarn, Message: fmt.Sprintf(format, args...)})
}

func (m *Migrator) progress(table string, processed, total int64, format string, args ...any) {
	m.emit(Event{
		Level:         EventProgress,
		Message:       fmt.Sprintf(format, args...),
		Table:         table,
		ProcessedRows: processed,
		TotalRows:     total,
	})
}

func (sm *SchemaMigrator) logf(format string, args ...any) {
	sm.emit(EventInfo, fmt.Sprintf(format, args...))
}

func (sm *SchemaMigrator) warnf(format string, args ...any) {
	sm.emit(EventWarn, fmt.Sprintf(format, args...))
}

func (sm *SchemaMigrator) emit(level EventLevel, message string) {
	event := Event{
		Time:    time.Now(),
		Level:   level,
		Message: strings.TrimRight(message, "\n"),
	}
	if sm.sink == nil {
		stdoutSink(event)
		return
	}
	sm.sink(event)
}
