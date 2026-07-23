package chatd

import (
	"context"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
)

// AgentLimiter admits chatd agentic loops. This package only defines
// the contract and invokes it at task and turn boundaries; the
// enforcing implementation lives in the enterprise-licensed
// enterprise/coderd/x/chatd package, where the license forbids
// modification.
type AgentLimiter interface {
	// NewLease is called once per runner and returns the lease that
	// brokers the chat's hold on a concurrent-agent slot.
	NewLease(chatID uuid.UUID) AgentSlotLease
}

// AgentLimiterOptions carries chatd-owned infrastructure into an
// AgentLimiterFactory.
type AgentLimiterOptions struct {
	Clock      quartz.Clock
	Logger     slog.Logger
	Registerer prometheus.Registerer
}

// AgentLimiterFactory constructs the chat worker's agent limiter. Nil
// leaves agentic loops uncapped.
type AgentLimiterFactory func(AgentLimiterOptions) AgentLimiter

// AgentSlotLease tracks one runner's hold on a concurrent-agent slot. A
// runner owns exactly one lease for its chat. The lease outlives the
// individual generation tasks that make up a turn: the first generation
// task of a turn acquires the slot, step boundaries keep it, and the
// slot is released once the turn ends and the last in-flight generation
// task exits.
type AgentSlotLease interface {
	// BeginTask and EndTask bracket each generation task goroutine so
	// releases requested mid-task are deferred until the task exits.
	BeginTask()
	EndTask()
	// EnsureHeld acquires the chat's agent slot unless the lease
	// already holds one, blocking until a slot frees or ctx is
	// canceled. A pending MarkTurnComplete release is honored first, so
	// a new turn queues behind other waiting chats.
	EnsureHeld(ctx context.Context) error
	// MarkTurnComplete flags the current turn's slot hold for release.
	// The release happens immediately when no generation task is
	// executing, and otherwise when the last in-flight task exits.
	MarkTurnComplete()
	// Close releases any held slot and permanently invalidates the
	// lease. Idempotent; the runner calls it unconditionally at
	// teardown.
	Close()
	// AttachToContext injects the lease's handle into a generation task
	// context so wait_agent can pause it and turn finishers can mark it
	// complete. Implementations may return ctx unchanged when every
	// handle method would be a no-op.
	AttachToContext(ctx context.Context) context.Context
}

// AgentSlotLeaseHandle is the narrow lease surface exposed to
// generation-task code (tool handlers, turn finishers) through the task
// context. Runner-lifecycle methods stay off it on purpose.
type AgentSlotLeaseHandle interface {
	// Pause yields the slot while the holder blocks on external
	// completion (wait_agent), freeing capacity for subagent children.
	// Reference counted: parallel tool calls share the chat's slot.
	Pause()
	// Resume undoes one Pause; the last Resume re-acquires the slot,
	// blocking until one frees. A canceled Resume leaves the lease
	// unheld and returns the context error; the next generation attempt
	// re-acquires through EnsureHeld.
	Resume(ctx context.Context) error
	MarkTurnComplete()
}

type agentSlotLeaseCtxKey struct{}

// WithAgentSlotLease exposes lease to generation-task code reached
// through ctx. AgentSlotLease.AttachToContext implementations use it;
// nothing else should.
func WithAgentSlotLease(ctx context.Context, lease AgentSlotLeaseHandle) context.Context {
	return context.WithValue(ctx, agentSlotLeaseCtxKey{}, lease)
}

// agentSlotLeaseFromContext returns the lease handle injected by the
// runner into generation task contexts. Absent on uncapped deployments
// and for non-generation callers; callers treat absence as a no-op.
func agentSlotLeaseFromContext(ctx context.Context) (AgentSlotLeaseHandle, bool) {
	lease, ok := ctx.Value(agentSlotLeaseCtxKey{}).(AgentSlotLeaseHandle)
	return lease, ok
}

// releaseAgentSlotOnTransition marks the turn complete on the lease in
// ctx. Turn finishers call it when their state transition callback
// succeeded, regardless of the surrounding Update error, because a
// post-commit publish failure returns an error after the transition is
// durably committed and the retry exits on the fence without another
// chance to release. A release after a commit failure is benign: the
// retrying task re-acquires through EnsureHeld.
func releaseAgentSlotOnTransition(ctx context.Context) {
	if lease, ok := agentSlotLeaseFromContext(ctx); ok {
		lease.MarkTurnComplete()
	}
}

func agentLimiterFromFactory(cfg Config, clk quartz.Clock) AgentLimiter {
	if cfg.AgentLimiterFactory == nil {
		return nopAgentLimiter{}
	}
	return cfg.AgentLimiterFactory(AgentLimiterOptions{
		Clock:      clk,
		Logger:     cfg.Logger.Named("chatworker"),
		Registerer: cfg.PrometheusRegistry,
	})
}

// nopAgentLimiter is the uncapped default used when no factory is
// configured.
type nopAgentLimiter struct{}

func (nopAgentLimiter) NewLease(uuid.UUID) AgentSlotLease { return nopAgentSlotLease{} }

type nopAgentSlotLease struct{}

func (nopAgentSlotLease) BeginTask()                       {}
func (nopAgentSlotLease) EndTask()                         {}
func (nopAgentSlotLease) EnsureHeld(context.Context) error { return nil }
func (nopAgentSlotLease) MarkTurnComplete()                {}
func (nopAgentSlotLease) Close()                           {}
func (nopAgentSlotLease) AttachToContext(ctx context.Context) context.Context {
	return ctx
}
