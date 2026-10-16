// Package agent provides a provider-neutral, fail-closed execution boundary
// for actions selected by an AI agent. It does not interpret prompts or call
// model APIs; applications supply an allowlisted action registry and policy.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const MaxIntentInputBytes = 64 << 10
const journalWriteTimeout = 5 * time.Second

const PermissionReconcile auth.Permission = "agent:reconcile"

var (
	ErrInvalidIntent       = errors.New("agent intent is invalid")
	ErrUnknownAction       = errors.New("agent action is not registered")
	ErrPolicyDenied        = errors.New("agent action denied by policy")
	ErrPermissionDenied    = errors.New("agent action permission denied")
	ErrApprovalDenied      = errors.New("agent action approval denied")
	ErrIntentReplayed      = errors.New("agent intent was already processed")
	ErrAuditUnavailable    = errors.New("agent execution journal unavailable")
	ErrExecutionFailed     = errors.New("agent action execution failed")
	ErrExecutionUnresolved = errors.New("agent action execution outcome unresolved")
	namePattern            = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,62}$`)
)

type Effect uint8

const (
	EffectUnknown Effect = iota
	EffectReadOnly
	EffectMutation
	EffectDestructive
	EffectExternal
)

type Outcome string

const (
	OutcomeDenied     Outcome = "denied"
	OutcomeSucceeded  Outcome = "succeeded"
	OutcomeFailed     Outcome = "failed"
	OutcomeUnresolved Outcome = "unresolved"
)

// Intent is a bounded, structured request for one registered action. ID is an
// idempotency key scoped to the authenticated actor and tenant.
type Intent struct {
	ID     uuid.UUID
	Action string
	Input  json.RawMessage
}

// Action is application code explicitly exposed to agents. Effects must be
// truthful: destructive and external actions require explicit approval.
type Action struct {
	Name       string
	Permission auth.Permission
	Effect     Effect
	Validate   func(context.Context, json.RawMessage) error
	Execute    func(context.Context, auth.Principal, json.RawMessage) (json.RawMessage, error)
}

type Decision struct {
	Allowed bool
}

type Policy interface {
	Evaluate(context.Context, auth.Principal, Intent, Action) (Decision, error)
}

type PolicyFunc func(context.Context, auth.Principal, Intent, Action) (Decision, error)

func (fn PolicyFunc) Evaluate(ctx context.Context, principal auth.Principal, intent Intent, action Action) (Decision, error) {
	if fn == nil {
		return Decision{}, errors.New("agent policy is nil")
	}
	return fn(ctx, principal, intent, action)
}

// Approver must verify an explicit human grant bound to this principal,
// intent ID, action, and tenant. It is called for destructive/external actions.
type Approver interface {
	Approve(context.Context, auth.Principal, Intent, Action) error
}

type ApproverFunc func(context.Context, auth.Principal, Intent, Action) error

func (fn ApproverFunc) Approve(ctx context.Context, principal auth.Principal, intent Intent, action Action) error {
	if fn == nil {
		return errors.New("agent approver is nil")
	}
	return fn(ctx, principal, intent, action)
}

// Attempt contains only identifiers, a bounded action name, and an effect
// class. Raw prompts and action input/output never enter the journal.
type Attempt struct {
	ExecutionID uuid.UUID
	IntentID    uuid.UUID
	ActorID     uuid.UUID
	TenantID    uuid.UUID
	Action      string
	Effect      Effect
}

func (a Attempt) Validate() error {
	if a.ExecutionID.Version() != 7 || a.ExecutionID.Variant() != 2 ||
		a.IntentID.Version() != 7 || a.IntentID.Variant() != 2 ||
		a.ActorID.Version() != 7 || a.ActorID.Variant() != 2 ||
		a.TenantID.Version() != 7 || a.TenantID.Variant() != 2 ||
		!namePattern.MatchString(a.Action) || a.Effect > EffectExternal {
		return ErrInvalidIntent
	}
	return nil
}

// Journal must durably create an attempt before execution and update that
// attempt afterward. Begin returns false for an already-seen intent; callers
// must never execute a replay. Rejections are recorded without executing.
type Journal interface {
	Begin(context.Context, Attempt) (created bool, err error)
	Reject(context.Context, Attempt, string) error
	Finish(context.Context, uuid.UUID, uuid.UUID, Outcome, string) error
}

type Engine struct {
	actions  map[string]Action
	policy   Policy
	approver Approver
	journal  Journal
}

func New(actions []Action, policy Policy, approver Approver, journal Journal) (*Engine, error) {
	if len(actions) == 0 {
		return nil, errors.New("at least one agent action is required")
	}
	if policy == nil {
		return nil, errors.New("agent policy is required")
	}
	if approver == nil {
		return nil, errors.New("agent approver is required")
	}
	if journal == nil {
		return nil, errors.New("durable agent execution journal is required")
	}
	registry := make(map[string]Action, len(actions))
	for _, action := range actions {
		if !namePattern.MatchString(action.Name) {
			return nil, fmt.Errorf("agent action name is invalid")
		}
		if _, exists := registry[action.Name]; exists {
			return nil, fmt.Errorf("duplicate agent action %q", action.Name)
		}
		if action.Effect < EffectReadOnly || action.Effect > EffectExternal {
			return nil, fmt.Errorf("agent action %q has an invalid effect", action.Name)
		}
		if _, err := auth.NewPermission(string(action.Permission)); err != nil {
			return nil, fmt.Errorf("agent action %q requires a valid permission", action.Name)
		}
		if action.Validate == nil || action.Execute == nil {
			return nil, fmt.Errorf("agent action %q requires validation and execution", action.Name)
		}
		registry[action.Name] = action
	}
	return &Engine{actions: registry, policy: policy, approver: approver, journal: journal}, nil
}

// Result carries the execution ID for correlation. Output is returned only to
// the caller and is never persisted by the engine.
type Result struct {
	ExecutionID uuid.UUID
	Output      json.RawMessage
}

// ActionError uses stable codes and safe messages. MayHaveExecuted is true
// after the executor starts; callers must reconcile rather than blindly retry.
type ActionError struct {
	Code            string
	ExecutionID     uuid.UUID
	MayHaveExecuted bool
	cause           error
}

func (e *ActionError) Error() string { return e.Code }

func (e *ActionError) Unwrap() error { return e.cause }

func (e *Engine) Execute(ctx context.Context, intent Intent) (Result, error) {
	if ctx == nil || !validIntent(intent) {
		return Result{}, &ActionError{Code: "intent.invalid", cause: ErrInvalidIntent}
	}
	principal, authenticated := auth.FromContext(ctx)
	if !authenticated || !validPrincipal(principal) {
		return Result{}, &ActionError{Code: "authentication.required", cause: auth.ErrCredentialsRequired}
	}
	tenant, tenantPresent := tenancy.FromContext(ctx)
	if !tenantPresent || tenant.ID != principal.Tenant().ID {
		return Result{}, &ActionError{Code: "tenant.context_mismatch", cause: tenancy.ErrInvalid}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	intent = cloneIntent(intent)
	action, registered := e.actions[intent.Action]
	attempt, err := newAttempt(principal, intent, action.Effect)
	if err != nil {
		return Result{}, &ActionError{Code: "execution_id.unavailable", cause: err}
	}
	if !registered {
		return Result{}, e.reject(ctx, attempt, ErrUnknownAction, "action.unknown")
	}
	decision, err := e.policy.Evaluate(ctx, principal, cloneIntent(intent), action)
	if err != nil {
		return Result{}, e.reject(ctx, attempt, err, "policy.failed")
	}
	if !decision.Allowed {
		return Result{}, e.reject(ctx, attempt, ErrPolicyDenied, "policy.denied")
	}
	if !json.Valid(intent.Input) {
		return Result{}, e.reject(ctx, attempt, ErrInvalidIntent, "intent.invalid")
	}
	if err := action.Validate(ctx, cloneJSON(intent.Input)); err != nil {
		return Result{}, e.reject(ctx, attempt, err, "intent.invalid")
	}
	if !principal.Can(action.Permission) {
		return Result{}, e.reject(ctx, attempt, ErrPermissionDenied, "authorization.denied")
	}
	if action.Effect == EffectDestructive || action.Effect == EffectExternal {
		if err := e.approver.Approve(ctx, principal, cloneIntent(intent), action); err != nil {
			return Result{}, e.reject(ctx, attempt, err, "approval.denied")
		}
	}
	beginCtx, cancel := context.WithTimeout(ctx, journalWriteTimeout)
	created, err := e.journal.Begin(beginCtx, attempt)
	cancel()
	if err != nil {
		return Result{}, &ActionError{Code: "audit.unavailable", ExecutionID: attempt.ExecutionID, cause: errors.Join(ErrAuditUnavailable, err)}
	}
	if !created {
		return Result{}, &ActionError{Code: "intent.replayed", cause: ErrIntentReplayed}
	}
	if err := ctx.Err(); err != nil {
		finishErr := e.finish(ctx, attempt, OutcomeFailed, "execution.cancelled")
		return Result{}, &ActionError{Code: "action.cancelled", ExecutionID: attempt.ExecutionID, cause: errors.Join(err, finishErr)}
	}
	output, executeErr := action.Execute(ctx, principal, cloneJSON(intent.Input))
	if executeErr != nil {
		finishErr := e.finish(ctx, attempt, OutcomeUnresolved, "execution.unresolved")
		return Result{}, &ActionError{Code: "action.failed", ExecutionID: attempt.ExecutionID, MayHaveExecuted: true, cause: errors.Join(ErrExecutionFailed, executeErr, finishErr)}
	}
	if len(output) > MaxIntentInputBytes || (len(output) > 0 && !json.Valid(output)) {
		finishErr := e.finish(ctx, attempt, OutcomeUnresolved, "output.invalid")
		return Result{}, &ActionError{Code: "output.invalid", ExecutionID: attempt.ExecutionID, MayHaveExecuted: true, cause: errors.Join(ErrExecutionFailed, finishErr)}
	}
	if err := e.finish(ctx, attempt, OutcomeSucceeded, ""); err != nil {
		return Result{}, &ActionError{Code: "audit.incomplete", ExecutionID: attempt.ExecutionID, MayHaveExecuted: true, cause: errors.Join(ErrExecutionUnresolved, err)}
	}
	return Result{ExecutionID: attempt.ExecutionID, Output: cloneJSON(output)}, nil
}

func (e *Engine) reject(ctx context.Context, attempt Attempt, cause error, code string) error {
	rejectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalWriteTimeout)
	err := e.journal.Reject(rejectCtx, attempt, code)
	cancel()
	if errors.Is(err, ErrIntentReplayed) {
		return &ActionError{Code: "intent.replayed", cause: errors.Join(ErrIntentReplayed, err)}
	}
	if err != nil {
		return &ActionError{Code: "audit.unavailable", ExecutionID: attempt.ExecutionID, cause: errors.Join(ErrAuditUnavailable, err)}
	}
	return &ActionError{Code: code, ExecutionID: attempt.ExecutionID, cause: errors.Join(rejectionSentinel(code), cause)}
}

func (e *Engine) finish(ctx context.Context, attempt Attempt, outcome Outcome, failureCode string) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalWriteTimeout)
	defer cancel()
	return e.journal.Finish(finishCtx, attempt.ExecutionID, attempt.ActorID, outcome, failureCode)
}

func rejectionSentinel(code string) error {
	switch code {
	case "action.unknown":
		return ErrUnknownAction
	case "policy.failed", "policy.denied":
		return ErrPolicyDenied
	case "intent.invalid":
		return ErrInvalidIntent
	case "authorization.denied":
		return ErrPermissionDenied
	case "approval.denied":
		return ErrApprovalDenied
	default:
		return errors.New("agent action rejected")
	}
}

func validPrincipal(principal auth.Principal) bool {
	subject := principal.SubjectID()
	tenantID := principal.Tenant().ID
	return subject.Version() == 7 && subject.Variant() == 2 && tenantID.Version() == 7 && tenantID.Variant() == 2
}

func validIntent(intent Intent) bool {
	return intent.ID.Version() == 7 && intent.ID.Variant() == 2 &&
		namePattern.MatchString(intent.Action) && len(intent.Input) <= MaxIntentInputBytes
}

func newAttempt(principal auth.Principal, intent Intent, effect Effect) (Attempt, error) {
	executionID, err := uuid.New()
	if err != nil {
		return Attempt{}, err
	}
	return Attempt{
		ExecutionID: executionID,
		IntentID:    intent.ID,
		ActorID:     principal.SubjectID(),
		TenantID:    principal.Tenant().ID,
		Action:      intent.Action,
		Effect:      effect,
	}, nil
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneIntent(intent Intent) Intent {
	intent.Input = cloneJSON(intent.Input)
	return intent
}
