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

	// Enter on the last field advances to the Continue button; enter on the
	// button validates everything and submits.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.focused != buttonIndex || got.submitted {
		t.Fatalf("expected focus on the Continue button before submission: %#v", got)
	}

	updated, command := got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got = updated.(model)
	if !got.submitted || got.cancelled || got.err != nil {
		t.Fatalf("expected valid submission: %#v", got)
	}
	if command == nil {
		t.Fatal("expected submission to quit the program")
	}
}

func TestContinueButtonJumpsToFirstInvalidInput(t *testing.T) {
	m := initialModel()
	// mysqlDatabase left empty: submission must bounce focus back to it.
	m.inputs[postgresDatabase].SetValue("target")
	m.focused = buttonIndex

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.submitted {
		t.Fatal("expected submission to be blocked by the empty database field")
	}
	if got.focused != mysqlDatabase {
		t.Fatalf("expected focus to jump to the invalid field, got %d", got.focused)
	}
	if got.err == nil {
		t.Fatal("expected a validation error to display")
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
