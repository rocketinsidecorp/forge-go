package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestSecurityEventValidation(t *testing.T) {
	valid := SecurityEvent{
		Kind:      SecurityLoginSucceeded,
		Outcome:   SecurityOutcomeSucceeded,
		SubjectID: uuid.MustNew(),
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}

	invalidKind := valid
	invalidKind.Kind = "contains space"
	if err := invalidKind.Validate(); err == nil {
		t.Fatal("invalid kind was accepted")
	}
	invalidOutcome := valid
	invalidOutcome.Outcome = "maybe"
	if err := invalidOutcome.Validate(); err == nil {
		t.Fatal("invalid outcome was accepted")
	}
	invalidID := valid
	invalidID.SubjectID = uuid.UUID{}
	invalidID.SubjectID[15] = 1
	if err := invalidID.Validate(); err == nil {
		t.Fatal("invalid subject ID was accepted")
	}
}

func TestSecurityAuditorFuncRejectsNil(t *testing.T) {
	var auditor SecurityAuditorFunc
	if err := auditor.RecordSecurityEvent(context.Background(), SecurityEvent{}); err == nil {
		t.Fatal("nil auditor function was accepted")
	}
	auditor = func(context.Context, SecurityEvent) error { return errors.New("boom") }
	if err := auditor.RecordSecurityEvent(context.Background(), SecurityEvent{}); err == nil {
		t.Fatal("auditor error was lost")
	}
}
