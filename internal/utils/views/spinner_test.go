package views

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSpinnerCancellationIsStoredOnTheModel(t *testing.T) {
	first := initialModel("first", true)
	updated, command := first.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	cancelled := updated.(model)

	if !cancelled.aborted || !cancelled.quitting {
		t.Fatalf("expected cancelled spinner state, got aborted=%v quitting=%v", cancelled.aborted, cancelled.quitting)
	}
	if command == nil {
		t.Fatal("expected cancellation to return a quit command")
	}

	second := initialModel("second", true)
	if second.aborted {
		t.Fatal("cancelling one spinner must not affect another spinner")
	}
}

func TestSpinnerStopMessageDoesNotReportCancellation(t *testing.T) {
	initial := initialModel("working", true)
	updated, command := initial.Update(Msg("quit"))
	stopped := updated.(model)

	if stopped.aborted || !stopped.quitting {
		t.Fatalf("expected normal stopped state, got aborted=%v quitting=%v", stopped.aborted, stopped.quitting)
	}
	if command == nil {
		t.Fatal("expected stop message to return a quit command")
	}
}
