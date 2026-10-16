package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestEngineExecutesOnlyAfterPolicyValidationAuthorizationAndJournal(t *testing.T) {
	t.Parallel()
	var steps []string
	principal := testPrincipal(t, "reports:read")
	journal := &testJournal{steps: &steps}
	action := Action{
		Name:       "report.read",
		Permission: "reports:read",
		Effect:     EffectReadOnly,
		Validate: func(_ context.Context, input json.RawMessage) error {
			steps = append(steps, "validate")
			if string(input) != `{"report":"daily"}` {
				t.Fatalf("validator received mutated input %s", input)
			}
			copy(input, []byte(`{"report":"changed"}`))
			return nil
		},
		Execute: func(_ context.Context, got auth.Principal, input json.RawMessage) (json.RawMessage, error) {
			steps = append(steps, "execute")
			if got.SubjectID() != principal.SubjectID() || string(input) != `{"report":"daily"}` {
				t.Fatalf("executor received principal=%v input=%s", got.SubjectID(), input)
			}
			return json.RawMessage(`{"rows":3}`), nil
		},
	}
	engine := newTestEngine(t, []Action{action}, PolicyFunc(func(_ context.Context, _ auth.Principal, intent Intent, _ Action) (Decision, error) {
		steps = append(steps, "policy")
		copy(intent.Input, []byte(`{"report":"changed"}`))
		return Decision{Allowed: true}, nil
	}), ApproverFunc(func(context.Context, auth.Principal, Intent, Action) error {
		steps = append(steps, "approval")
		return nil
	}), journal)

	result, err := engine.Execute(authenticatedContext(t, principal), Intent{ID: uuid.MustNew(), Action: action.Name, Input: json.RawMessage(`{"report":"daily"}`)})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if string(result.Output) != `{"rows":3}` || result.ExecutionID == (uuid.UUID{}) {
		t.Fatalf("unexpected result: %+v", result)
	}
	if want := []string{"policy", "validate", "begin", "execute", "finish:succeeded"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps=%v, want %v", steps, want)
	}
	if journal.attempt.Action != action.Name || journal.attempt.IntentID == (uuid.UUID{}) {
		t.Fatal("journal did not receive action identity metadata")
	}
}

func TestEngineRequiresPermissionAndApprovalForDestructiveActions(t *testing.T) {
	t.Parallel()
	principal := testPrincipal(t, "data:delete")
	executed := false
	journal := &testJournal{}
	engine := newTestEngine(t, []Action{{
		Name: "data.delete", Permission: "data:delete", Effect: EffectDestructive,
		Validate: func(context.Context, json.RawMessage) error { return nil },
		Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
			executed = true
			return nil, nil
		},
	}}, PolicyFunc(func(context.Context, auth.Principal, Intent, Action) (Decision, error) {
		return Decision{Allowed: true}, nil
	}), ApproverFunc(func(_ context.Context, got auth.Principal, intent Intent, action Action) error {
		if got.SubjectID() != principal.SubjectID() || intent.ID == (uuid.UUID{}) || action.Effect != EffectDestructive {
			t.Fatal("approval was not bound to the actor, intent, and effect")
		}
		return errors.New("human approval missing")
	}), journal)

	_, err := engine.Execute(authenticatedContext(t, principal), Intent{ID: uuid.MustNew(), Action: "data.delete", Input: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrApprovalDenied) || executed {
		t.Fatalf("err=%v executed=%v, want approval denial without execution", err, executed)
	}
	if len(journal.rejected) != 1 || journal.rejected[0] != "approval.denied" {
		t.Fatalf("rejections=%v", journal.rejected)
	}
}

func TestEngineFailsClosedOnPolicyValidationPermissionAndJournalErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		permissions []auth.Permission
		policy      Policy
		validate    func(context.Context, json.RawMessage) error
		beginErr    error
		want        error
		wantReject  string
	}{
		{name: "policy", permissions: []auth.Permission{"items:read"}, policy: PolicyFunc(func(context.Context, auth.Principal, Intent, Action) (Decision, error) {
			return Decision{}, errors.New("private policy detail")
		}), want: ErrPolicyDenied, wantReject: "policy.failed"},
		{name: "policy deny", permissions: []auth.Permission{"items:read"}, policy: PolicyFunc(func(context.Context, auth.Principal, Intent, Action) (Decision, error) {
			return Decision{Allowed: false}, nil
		}), want: ErrPolicyDenied, wantReject: "policy.denied"},
		{name: "validation", permissions: []auth.Permission{"items:read"}, policy: allowPolicy(), validate: func(context.Context, json.RawMessage) error { return errors.New("private input detail") }, want: ErrInvalidIntent, wantReject: "intent.invalid"},
		{name: "permission", policy: allowPolicy(), want: ErrPermissionDenied, wantReject: "authorization.denied"},
		{name: "journal", permissions: []auth.Permission{"items:read"}, policy: allowPolicy(), beginErr: errors.New("database secret"), want: ErrAuditUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			executed := false
			journal := &testJournal{beginErr: tt.beginErr}
			validate := tt.validate
			if validate == nil {
				validate = func(context.Context, json.RawMessage) error { return nil }
			}
			engine := newTestEngine(t, []Action{{
				Name: "items.read", Permission: "items:read", Effect: EffectReadOnly,
				Validate: validate,
				Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
					executed = true
					return nil, nil
				},
			}}, tt.policy, noApproval(), journal)
			_, err := engine.Execute(authenticatedContext(t, testPrincipal(t, tt.permissions...)), Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)})
			if !errors.Is(err, tt.want) || executed {
				t.Fatalf("err=%v executed=%v, want errors.Is(_, %v) and no execution", err, executed, tt.want)
			}
			if tt.wantReject != "" && (len(journal.rejected) != 1 || journal.rejected[0] != tt.wantReject) {
				t.Fatalf("rejections=%v, want [%s]", journal.rejected, tt.wantReject)
			}
			if tt.name == "journal" && executed {
				t.Fatal("action ran without durable Begin")
			}
		})
	}
}

func TestEngineDoesNotExecuteReplayedIntent(t *testing.T) {
	t.Parallel()
	journal := &testJournal{duplicate: true}
	executed := false
	engine := newTestEngine(t, []Action{{
		Name: "items.read", Permission: "items:read", Effect: EffectReadOnly,
		Validate: func(context.Context, json.RawMessage) error { return nil },
		Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
			executed = true
			return nil, nil
		},
	}}, allowPolicy(), noApproval(), journal)
	_, err := engine.Execute(authenticatedContext(t, testPrincipal(t, "items:read")), Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrIntentReplayed) || executed {
		t.Fatalf("err=%v executed=%v, want replay refusal", err, executed)
	}
}

func TestEngineJournalsMalformedJSONAndRefusesExecution(t *testing.T) {
	t.Parallel()
	journal := &testJournal{}
	executed := false
	validated := false
	engine := newTestEngine(t, []Action{{
		Name: "items.read", Permission: "items:read", Effect: EffectReadOnly,
		Validate: func(context.Context, json.RawMessage) error {
			validated = true
			return nil
		},
		Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
			executed = true
			return nil, nil
		},
	}}, allowPolicy(), noApproval(), journal)
	_, err := engine.Execute(authenticatedContext(t, testPrincipal(t, "items:read")), Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{`)})
	if !errors.Is(err, ErrInvalidIntent) || validated || executed {
		t.Fatalf("err=%v validated=%v executed=%v", err, validated, executed)
	}
	if !reflect.DeepEqual(journal.rejected, []string{"intent.invalid"}) || journal.attempt.IntentID == (uuid.UUID{}) {
		t.Fatalf("malformed intent was not safely journaled: rejected=%v attempt=%+v", journal.rejected, journal.attempt)
	}
}

func TestEngineMarksExecutionAndAuditFailuresAsPotentiallyApplied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		execErr   error
		finishErr error
		want      error
	}{
		{name: "executor", execErr: errors.New("partial external write"), want: ErrExecutionFailed},
		{name: "final audit", finishErr: errors.New("database unavailable"), want: ErrExecutionUnresolved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			journal := &testJournal{finishErr: tt.finishErr}
			engine := newTestEngine(t, []Action{{
				Name: "storage.write", Permission: "storage:write", Effect: EffectMutation,
				Validate: func(context.Context, json.RawMessage) error { return nil },
				Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
					return nil, tt.execErr
				},
			}}, allowPolicy(), noApproval(), journal)
			_, err := engine.Execute(authenticatedContext(t, testPrincipal(t, "storage:write")), Intent{ID: uuid.MustNew(), Action: "storage.write", Input: json.RawMessage(`{}`)})
			var actionErr *ActionError
			if !errors.Is(err, tt.want) || !errors.As(err, &actionErr) || !actionErr.MayHaveExecuted {
				t.Fatalf("err=%v, want applied-uncertain error matching %v", err, tt.want)
			}
		})
	}
}

func TestEngineRequiresMiddlewareAuthenticatedPrincipalAndMatchingTenant(t *testing.T) {
	t.Parallel()
	principal := testPrincipal(t, "items:read")
	executed := false
	journal := &testJournal{}
	engine := newTestEngine(t, []Action{{
		Name: "items.read", Permission: "items:read", Effect: EffectReadOnly,
		Validate: func(context.Context, json.RawMessage) error { return nil },
		Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) {
			executed = true
			return nil, nil
		},
	}}, allowPolicy(), noApproval(), journal)
	intent := Intent{ID: uuid.MustNew(), Action: "items.read", Input: json.RawMessage(`{}`)}
	if _, err := engine.Execute(context.Background(), intent); !errors.Is(err, auth.ErrCredentialsRequired) {
		t.Fatalf("unauthenticated Execute()=%v, want authentication required", err)
	}
	otherTenant, _ := tenancy.New(uuid.MustNew())
	ctx, err := tenancy.WithContext(authenticatedContext(t, principal), otherTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Execute(ctx, intent); err == nil {
		t.Fatal("Execute() accepted a tenant context that disagrees with the authenticated principal")
	}
	if executed || len(journal.rejected) != 0 || journal.attempt != (Attempt{}) {
		t.Fatalf("untrusted context reached execution or journal: executed=%v rejected=%v attempt=%+v", executed, journal.rejected, journal.attempt)
	}
}

func TestNewRejectsMissingSafetyDependenciesAndInvalidActions(t *testing.T) {
	t.Parallel()
	action := Action{Name: "items.read", Permission: "items:read", Effect: EffectReadOnly, Validate: func(context.Context, json.RawMessage) error { return nil }, Execute: func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	journal := &testJournal{}
	if _, err := New([]Action{action}, nil, noApproval(), journal); err == nil {
		t.Fatal("engine without policy was accepted")
	}
	if _, err := New([]Action{action}, allowPolicy(), nil, journal); err == nil {
		t.Fatal("engine without approver was accepted")
	}
	if _, err := New([]Action{action}, allowPolicy(), noApproval(), nil); err == nil {
		t.Fatal("engine without durable journal was accepted")
	}
	bad := action
	bad.Execute = nil
	if _, err := New([]Action{bad}, allowPolicy(), noApproval(), journal); err == nil {
		t.Fatal("action without executor was accepted")
	}
}

func newTestEngine(t *testing.T, actions []Action, policy Policy, approver Approver, journal Journal) *Engine {
	t.Helper()
	engine, err := New(actions, policy, approver, journal)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	return engine
}

func testPrincipal(t *testing.T, permissions ...auth.Permission) auth.Principal {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, permissions...)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func authenticatedContext(t *testing.T, principal auth.Principal) context.Context {
	t.Helper()
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		if digest != token.Digest() {
			return auth.Session{}, false, nil
		}
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	var authenticated context.Context
	handler := middleware.Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		authenticated = r.Context()
	}))
	request := httptest.NewRequest(http.MethodGet, "/agent-test", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if authenticated == nil {
		t.Fatal("authentication middleware did not establish a principal")
	}
	return authenticated
}

func allowPolicy() Policy {
	return PolicyFunc(func(context.Context, auth.Principal, Intent, Action) (Decision, error) {
		return Decision{Allowed: true}, nil
	})
}

func noApproval() Approver {
	return ApproverFunc(func(context.Context, auth.Principal, Intent, Action) error { return nil })
}

type testJournal struct {
	steps     *[]string
	attempt   Attempt
	rejected  []string
	beginErr  error
	finishErr error
	duplicate bool
}

func (j *testJournal) Begin(_ context.Context, attempt Attempt) (bool, error) {
	if j.steps != nil {
		*j.steps = append(*j.steps, "begin")
	}
	j.attempt = attempt
	return !j.duplicate, j.beginErr
}

func (j *testJournal) Reject(_ context.Context, attempt Attempt, code string) error {
	j.attempt = attempt
	j.rejected = append(j.rejected, code)
	return nil
}

func (j *testJournal) Finish(_ context.Context, _, _ uuid.UUID, outcome Outcome, _ string) error {
	if j.steps != nil {
		*j.steps = append(*j.steps, "finish:"+string(outcome))
	}
	return j.finishErr
}
