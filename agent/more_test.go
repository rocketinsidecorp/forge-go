package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func readAction(name string) Action {
	return Action{
		Name: name, Permission: "items:read", Effect: EffectReadOnly,
		Validate: func(context.Context, json.RawMessage) error { return nil },
		Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
	}
}

func TestNilPolicyAndApproverFuncsFailClosed(t *testing.T) {
	t.Parallel()
	principal := testPrincipal(t)
	if decision, err := PolicyFunc(nil).Evaluate(t.Context(), principal, Intent{}, Action{}); err == nil || decision.Allowed {
		t.Errorf("a nil policy decided: %+v %v", decision, err)
	}
	if err := ApproverFunc(nil).Approve(t.Context(), principal, Intent{}, Action{}); err == nil {
		t.Error("a nil approver approved")
	}
}

func TestNewRejectsInvalidActionRegistries(t *testing.T) {
	t.Parallel()
	journal := &testJournal{}
	build := func(actions ...Action) error {
		_, err := New(actions, allowPolicy(), noApproval(), journal)
		return err
	}
	if err := build(); err == nil {
		t.Error("an engine without actions was accepted")
	}
	if err := build(readAction("Bad Name")); err == nil {
		t.Error("an invalid action name was accepted")
	}
	if err := build(readAction("items.read"), readAction("items.read")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: %v", err)
	}
	unknownEffect := readAction("items.odd")
	unknownEffect.Effect = EffectUnknown
	if err := build(unknownEffect); err == nil || !strings.Contains(err.Error(), "invalid effect") {
		t.Errorf("unknown effect: %v", err)
	}
	tooFar := readAction("items.far")
	tooFar.Effect = EffectExternal + 1
	if err := build(tooFar); err == nil {
		t.Error("an effect beyond the known ones was accepted")
	}
	noPermission := readAction("items.open")
	noPermission.Permission = "OPEN"
	if err := build(noPermission); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Errorf("invalid permission: %v", err)
	}
	noValidate := readAction("items.unchecked")
	noValidate.Validate = nil
	if err := build(noValidate); err == nil {
		t.Error("an action without validation was accepted")
	}
}

func TestActionErrorIsItsCodeAndUnwrapsItsCause(t *testing.T) {
	t.Parallel()
	err := &ActionError{Code: "policy.denied", cause: ErrPolicyDenied}
	if err.Error() != "policy.denied" || !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("err = %v", err)
	}
}

func TestAttemptValidateRefusesEveryMalformedField(t *testing.T) {
	t.Parallel()
	valid := Attempt{ExecutionID: uuid.MustNew(), IntentID: uuid.MustNew(), ActorID: uuid.MustNew(), TenantID: uuid.MustNew(), Action: "items.read", Effect: EffectReadOnly}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Attempt){
		"execution": func(a *Attempt) { a.ExecutionID = uuid.UUID{} },
		"intent":    func(a *Attempt) { a.IntentID = uuid.UUID{} },
		"actor":     func(a *Attempt) { a.ActorID = uuid.UUID{} },
		"tenant":    func(a *Attempt) { a.TenantID = uuid.UUID{} },
		"action":    func(a *Attempt) { a.Action = "X" },
		"effect":    func(a *Attempt) { a.Effect = EffectExternal + 1 },
	} {
		attempt := valid
		mutate(&attempt)
		if err := attempt.Validate(); !errors.Is(err, ErrInvalidIntent) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// replayJournal answers Reject and Finish with chosen errors.
type replayJournal struct {
	testJournal
	rejectErr error
}

func (j *replayJournal) Reject(ctx context.Context, attempt Attempt, code string) error {
	_ = j.testJournal.Reject(ctx, attempt, code)
	return j.rejectErr
}

func TestEngineReportsJournalFailuresOnRejection(t *testing.T) {
	t.Parallel()
	principal := testPrincipal(t, "items:read")
	execute := func(journal Journal, intentAction string) error {
		engine := newTestEngine(t, []Action{readAction("items.read")}, allowPolicy(), noApproval(), journal)
		_, err := engine.Execute(authenticatedContext(t, principal), Intent{ID: uuid.MustNew(), Action: intentAction, Input: json.RawMessage(`{}`)})
		return err
	}
	// An unknown action whose rejection cannot be journaled is an audit
	// failure, never a silent refusal.
	err := execute(&replayJournal{rejectErr: errors.New("journal down")}, "items.write")
	var actionErr *ActionError
	if !errors.As(err, &actionErr) || actionErr.Code != "audit.unavailable" || !errors.Is(err, ErrAuditUnavailable) {
		t.Errorf("journal down: %v", err)
	}
	// A rejection the journal says was a replay is reported as one.
	err = execute(&replayJournal{rejectErr: ErrIntentReplayed}, "items.write")
	if !errors.As(err, &actionErr) || actionErr.Code != "intent.replayed" || !errors.Is(err, ErrIntentReplayed) {
		t.Errorf("replayed rejection: %v", err)
	}
	// A plain unknown action is journaled and refused.
	journal := &replayJournal{}
	err = execute(journal, "items.write")
	if !errors.Is(err, ErrUnknownAction) || len(journal.rejected) != 1 || journal.rejected[0] != "action.unknown" {
		t.Errorf("unknown action: %v rejected=%v", err, journal.rejected)
	}
}

func TestEngineRefusesInvalidIntentsWithoutTouchingTheJournal(t *testing.T) {
	t.Parallel()
	journal := &testJournal{}
	engine := newTestEngine(t, []Action{readAction("items.read")}, allowPolicy(), noApproval(), journal)
	ctx := authenticatedContext(t, testPrincipal(t, "items:read"))
	for name, intent := range map[string]Intent{
		"no id":      {Action: "items.read"},
		"bad action": {ID: uuid.MustNew(), Action: "X"},
		"huge input": {ID: uuid.MustNew(), Action: "items.read", Input: make([]byte, MaxIntentInputBytes+1)},
	} {
		_, err := engine.Execute(ctx, intent)
		var actionErr *ActionError
		if !errors.As(err, &actionErr) || actionErr.Code != "intent.invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	var nilCtx context.Context
	if _, err := engine.Execute(nilCtx, Intent{ID: uuid.MustNew(), Action: "items.read"}); err == nil {
		t.Error("a nil context was accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := engine.Execute(canceled, Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: %v", err)
	}
	if journal.attempt != (Attempt{}) {
		t.Error("the journal was touched for an invalid intent")
	}
}

// cancelOnBegin cancels the request while the attempt is being journaled.
type cancelOnBegin struct {
	testJournal
	cancel context.CancelFunc
	steps  []string
}

func (j *cancelOnBegin) Begin(ctx context.Context, attempt Attempt) (bool, error) {
	j.cancel()
	return j.testJournal.Begin(ctx, attempt)
}

func (j *cancelOnBegin) Finish(ctx context.Context, execution, actor uuid.UUID, outcome Outcome, code string) error {
	j.steps = append(j.steps, string(outcome)+":"+code)
	return j.testJournal.Finish(ctx, execution, actor, outcome, code)
}

func TestEngineDoesNotExecuteAfterTheRequestIsCanceledMidJournal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(authenticatedContext(t, testPrincipal(t, "items:read")))
	defer cancel()
	journal := &cancelOnBegin{cancel: cancel}
	executed := false
	action := readAction("items.read")
	action.Execute = func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
		executed = true
		return nil, nil
	}
	engine := newTestEngine(t, []Action{action}, allowPolicy(), noApproval(), journal)
	_, err := engine.Execute(ctx, Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)})
	var actionErr *ActionError
	if !errors.As(err, &actionErr) || actionErr.Code != "action.cancelled" || actionErr.MayHaveExecuted || executed {
		t.Fatalf("err=%v executed=%v", err, executed)
	}
	if len(journal.steps) != 1 || journal.steps[0] != "failed:execution.cancelled" {
		t.Errorf("journal steps = %v, want the attempt closed as failed", journal.steps)
	}
}

func TestEngineTreatsOversizedOrInvalidOutputAsUnresolved(t *testing.T) {
	t.Parallel()
	for name, output := range map[string]json.RawMessage{
		"not json": json.RawMessage(`{broken`),
		"too big":  json.RawMessage(`"` + strings.Repeat("x", MaxIntentInputBytes) + `"`),
	} {
		journal := &cancelOnBegin{cancel: func() {}}
		action := readAction("items.read")
		action.Execute = func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) { return output, nil }
		engine := newTestEngine(t, []Action{action}, allowPolicy(), noApproval(), journal)
		result, err := engine.Execute(authenticatedContext(t, testPrincipal(t, "items:read")), Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)})
		var actionErr *ActionError
		if !errors.As(err, &actionErr) || actionErr.Code != "output.invalid" || !actionErr.MayHaveExecuted || len(result.Output) != 0 {
			t.Errorf("%s: result=%+v err=%v", name, result, err)
		}
		if len(journal.steps) != 1 || journal.steps[0] != "unresolved:output.invalid" {
			t.Errorf("%s: journal steps = %v", name, journal.steps)
		}
	}
}

func TestEngineReturnsACopyOfTheOutput(t *testing.T) {
	t.Parallel()
	shared := json.RawMessage(`{"n":1}`)
	action := readAction("items.read")
	action.Execute = func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) { return shared, nil }
	engine := newTestEngine(t, []Action{action}, allowPolicy(), noApproval(), &testJournal{})
	result, err := engine.Execute(authenticatedContext(t, testPrincipal(t, "items:read")), Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	result.Output[2] = 'X'
	if string(shared) != `{"n":1}` {
		t.Errorf("the caller changed the action's own buffer: %s", shared)
	}
}
