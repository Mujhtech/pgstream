package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/mujhtech/pgstream/internal/migrator"
)

// maxBufferedEvents bounds the per-run replay buffer. When the buffer is
// full the oldest events are dropped; late subscribers still get everything
// the buffer retains, addressed by stable absolute event IDs.
const maxBufferedEvents = 10000

// subscriberBuffer is the per-subscriber channel capacity. A subscriber that
// cannot drain this many events is disconnected and can reconnect with its
// last seen event ID to replay from the buffer.
const subscriberBuffer = 256

type runStatus string

const (
	runStatusRunning   runStatus = "running"
	runStatusSucceeded runStatus = "succeeded"
	runStatusFailed    runStatus = "failed"
)

type eventEnvelope struct {
	// ID is the session store's row id for this event, shared with the
	// storage-tail stream so Last-Event-ID reconnects work across both.
	ID    int64          `json:"id"`
	Event migrator.Event `json:"event"`
}

// run tracks one in-flight or finished migration and its event history.
type run struct {
	sessionID string

	mu          sync.Mutex
	events      []eventEnvelope
	subscribers map[chan eventEnvelope]struct{}
	status      runStatus
	err         error
	cancel      context.CancelFunc
}

func (r *run) publish(id int64, event migrator.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	envelope := eventEnvelope{ID: id, Event: event}
	r.events = append(r.events, envelope)
	if len(r.events) > maxBufferedEvents {
		r.events = r.events[len(r.events)-maxBufferedEvents:]
	}

	for subscriber := range r.subscribers {
		select {
		case subscriber <- envelope:
		default:
			// The subscriber lagged too far behind; disconnect it. It can
			// reconnect and replay from its last received ID.
			delete(r.subscribers, subscriber)
			close(subscriber)
		}
	}
}

// subscribe returns the buffered events after fromID plus a live channel.
// The caller must invoke the returned cancel function exactly once.
func (r *run) subscribe(fromID int64) ([]eventEnvelope, <-chan eventEnvelope, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var replay []eventEnvelope
	for _, envelope := range r.events {
		if envelope.ID >= fromID {
			replay = append(replay, envelope)
		}
	}

	subscriber := make(chan eventEnvelope, subscriberBuffer)
	if r.status != runStatusRunning {
		// The run already finished: hand back the buffered history and a
		// closed channel so the stream ends after the replay.
		close(subscriber)
		return replay, subscriber, func() {}
	}
	r.subscribers[subscriber] = struct{}{}

	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, active := r.subscribers[subscriber]; active {
			delete(r.subscribers, subscriber)
			close(subscriber)
		}
	}
	return replay, subscriber, cancel
}

// finish records the terminal state and closes every live subscriber so
// their event streams terminate. Callers must publish any final event
// before calling finish.
func (r *run) finish(err error) {
	r.mu.Lock()
	if err != nil {
		r.status = runStatusFailed
		r.err = err
	} else {
		r.status = runStatusSucceeded
	}
	for subscriber := range r.subscribers {
		delete(r.subscribers, subscriber)
		close(subscriber)
	}
	r.mu.Unlock()
}

func (r *run) snapshot() (runStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.err
}

// runRegistry indexes runs by session ID and enforces one run per session.
type runRegistry struct {
	mu   sync.Mutex
	runs map[string]*run
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: make(map[string]*run)}
}

// begin registers a new run for the session. It fails while a previous run
// for the same session is still in progress.
func (g *runRegistry) begin(sessionID string, cancel context.CancelFunc) (*run, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	newRun := &run{
		sessionID:   sessionID,
		subscribers: make(map[chan eventEnvelope]struct{}),
		status:      runStatusRunning,
		cancel:      cancel,
	}
	if existing, exists := g.runs[sessionID]; exists {
		status, _ := existing.snapshot()
		if status == runStatusRunning {
			return nil, fmt.Errorf("session %s already has a migration in progress", sessionID)
		}
	}
	g.runs[sessionID] = newRun
	return newRun, nil
}

func (g *runRegistry) get(sessionID string) *run {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs[sessionID]
}
