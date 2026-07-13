package session

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPortValidatorChecksNetworkRange(t *testing.T) {
	for _, value := range []string{"0", "65536", "-1", "not-a-port"} {
		if err := portValidator(value); err == nil {
			t.Fatalf("expected port %q to fail validation", value)
		}
	}
	if err := portValidator("5432"); err != nil {
		t.Fatalf("expected valid port: %v", err)
	}
}

func TestEnterDoesNotAdvancePastInvalidInput(t *testing.T) {
	m := initialModel()
	m.inputs[mysqlHost].SetValue("")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.focused != mysqlHost {
		t.Fatalf("focus advanced to %d despite invalid host", got.focused)
	}
	if got.err == nil || got.submitted {
		t.Fatalf("expected validation error without submission: %#v", got)
	}
}

func TestFinalEnterRequiresAllInputsAndMarksSubmission(t *testing.T) {
	m := initialModel()
	m.inputs[mysqlDatabase].SetValue("source")
	m.inputs[postgresDatabase].SetValue("target")
	m.focused = postgresSchema

	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if !got.submitted || got.cancelled || got.err != nil {
		t.Fatalf("expected valid submission: %#v", got)
	}
	if command == nil {
		t.Fatal("expected submission to quit the program")
	}
}

func TestEscapeMarksSessionAsCancelled(t *testing.T) {
	m := initialModel()
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(model)
	if !got.cancelled || got.submitted {
		t.Fatalf("expected cancellation: %#v", got)
	}
	if command == nil {
		t.Fatal("expected cancellation to quit the program")
	}
}
