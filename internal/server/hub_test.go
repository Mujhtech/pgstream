package server

import (
	"context"
	"testing"

	"github.com/mujhtech/pgstream/internal/migrator"
)

func TestRunPublishReplaysToLateSubscribers(t *testing.T) {
	registry := newRunRegistry()
	activeRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	activeRun.publish(1, migrator.Event{Level: migrator.EventInfo, Message: "first"})
	activeRun.publish(2, migrator.Event{Level: migrator.EventInfo, Message: "second"})

	replay, live, cancel := activeRun.subscribe(0)
	defer cancel()

	if len(replay) != 2 {
		t.Fatalf("expected 2 replayed events, got %d", len(replay))
	}
	if replay[0].ID != 1 || replay[0].Event.Message != "first" {
		t.Fatalf("unexpected first replayed event: %#v", replay[0])
	}

	activeRun.publish(3, migrator.Event{Level: migrator.EventWarn, Message: "third"})
	envelope := <-live
	if envelope.ID != 3 || envelope.Event.Message != "third" {
		t.Fatalf("unexpected live event: %#v", envelope)
	}
}

func TestRunSubscribeFromLastSeenID(t *testing.T) {
	registry := newRunRegistry()
	activeRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	for i := int64(1); i <= 5; i++ {
		activeRun.publish(i, migrator.Event{Level: migrator.EventInfo, Message: "event"})
	}

	replay, _, cancel := activeRun.subscribe(4)
	defer cancel()
	if len(replay) != 2 {
		t.Fatalf("expected replay of events 4 and 5, got %d events", len(replay))
	}
	if replay[0].ID != 4 {
		t.Fatalf("expected replay to start at ID 4, got %d", replay[0].ID)
	}
}

func TestRegistryRejectsConcurrentRunsPerSession(t *testing.T) {
	registry := newRunRegistry()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	firstRun, err := registry.begin("session-1", cancel)
	if err != nil {
		t.Fatalf("begin first run: %v", err)
	}
	if _, err := registry.begin("session-1", cancel); err == nil {
		t.Fatal("expected a second concurrent run to be rejected")
	}

	firstRun.finish(nil)
	if _, err := registry.begin("session-1", cancel); err != nil {
		t.Fatalf("expected a new run after the first finished, got %v", err)
	}

	if _, err := registry.begin("session-2", cancel); err != nil {
		t.Fatalf("independent sessions must run concurrently, got %v", err)
	}
}

func TestFinishClosesLiveSubscribers(t *testing.T) {
	registry := newRunRegistry()
	activeRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	activeRun.publish(1, migrator.Event{Level: migrator.EventInfo, Message: "working"})

	_, live, cancel := activeRun.subscribe(0)
	defer cancel()

	activeRun.publish(2, migrator.Event{Level: migrator.EventError, Message: "terminal"})
	activeRun.finish(context.Canceled)

	if envelope := <-live; envelope.Event.Message != "terminal" {
		t.Fatalf("expected the terminal event before close, got %#v", envelope)
	}
	if _, open := <-live; open {
		t.Fatal("expected the subscriber channel to be closed after finish")
	}

	status, runErr := activeRun.snapshot()
	if status != runStatusFailed || runErr == nil {
		t.Fatalf("unexpected terminal state: %v %v", status, runErr)
	}
}

func TestSubscribeAfterFinishReplaysAndCloses(t *testing.T) {
	registry := newRunRegistry()
	activeRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	activeRun.publish(1, migrator.Event{Level: migrator.EventInfo, Message: "done"})
	activeRun.finish(nil)

	replay, live, cancel := activeRun.subscribe(0)
	defer cancel()
	if len(replay) != 1 || replay[0].Event.Message != "done" {
		t.Fatalf("expected full replay for a finished run, got %#v", replay)
	}
	if _, open := <-live; open {
		t.Fatal("expected an immediately closed channel for a finished run")
	}
}

func TestStorageIDsCarryAcrossRuns(t *testing.T) {
	// IDs come from the session store, so a new run's events always sort
	// after the previous run's and Last-Event-ID reconnects never mask them.
	registry := newRunRegistry()
	firstRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin first run: %v", err)
	}
	firstRun.publish(10, migrator.Event{Level: migrator.EventInfo, Message: "run1"})
	firstRun.finish(nil)

	secondRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin second run: %v", err)
	}
	secondRun.publish(11, migrator.Event{Level: migrator.EventInfo, Message: "run2"})

	replay, _, cancel := secondRun.subscribe(11)
	defer cancel()
	if len(replay) != 1 || replay[0].ID != 11 || replay[0].Event.Message != "run2" {
		t.Fatalf("expected run 2's event under its storage ID, got %#v", replay)
	}
}

func TestLaggingSubscriberIsDisconnected(t *testing.T) {
	registry := newRunRegistry()
	activeRun, err := registry.begin("session-1", func() {})
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}

	_, live, cancel := activeRun.subscribe(0)
	defer cancel()

	// Overflow the subscriber buffer without draining it.
	for i := 0; i < subscriberBuffer+10; i++ {
		activeRun.publish(int64(i+1), migrator.Event{Level: migrator.EventInfo, Message: "flood"})
	}

	drained := 0
	for range live {
		drained++
	}
	if drained != subscriberBuffer {
		t.Fatalf("expected the channel to be closed after %d buffered events, drained %d", subscriberBuffer, drained)
	}
}
