package migrator

import "testing"

func TestWithPhaseAcceptsKnownPhases(t *testing.T) {
	for _, phase := range []Phase{PhaseAll, PhaseSchemaOnly, PhaseDataOnly} {
		m := &Migrator{}
		if err := WithPhase(phase)(m); err != nil {
			t.Fatalf("WithPhase(%q): %v", phase, err)
		}
		if m.phase != phase {
			t.Fatalf("phase = %q, want %q", m.phase, phase)
		}
	}
}

func TestWithPhaseRejectsUnknownPhase(t *testing.T) {
	if err := WithPhase("everything")(&Migrator{}); err == nil {
		t.Fatal("expected error for unknown phase")
	}
}
