package chatd

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"github.com/coder/coder/v2/coderd/entitlements"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/quartz"
)

// MaxConcurrentAgents is the deployment-wide (per coderd replica) cap on
// concurrently executing chatd agentic loops when the deployment is not
// entitled to codersdk.FeatureUnlimitedChatAgents. A chat holds one slot
// for the duration of one turn; chats over the limit queue until a slot
// frees. Chat subagents are ordinary chats and count against the cap;
// a parent yields its slot while blocked in wait_agent so children can
// run. "Agents" here means chatd agentic loops, not workspace agents or
// managed agents.
const MaxConcurrentAgents = 3

// defaultEntitlementRecheckInterval bounds how long a blocked acquire
// waits before re-checking the entitlement bypass, so a license
// installed while chats queue unblocks them within seconds without any
// entitlement-change plumbing.
const defaultEntitlementRecheckInterval = 10 * time.Second

var errAgentSlotLeaseClosed = xerrors.New("chatd: agent slot lease closed")

type agentLimiterOptions struct {
	// Entitlements is consulted at every acquire attempt; nil defaults
	// to an unlicensed set, so the cap applies.
	Entitlements *entitlements.Set
	Clock        quartz.Clock
	Logger       slog.Logger
	// Registerer registers the limiter metrics when non-nil.
	Registerer prometheus.Registerer
	// Capacity overrides MaxConcurrentAgents; used by tests.
	Capacity int64
	// EntitlementRecheckInterval overrides the blocked-acquire recheck
	// cadence; used by tests.
	EntitlementRecheckInterval time.Duration
}

// agentLimiter enforces MaxConcurrentAgents across all runners of one
// chatd worker. The semaphore is process-local and ephemeral: a
// restarted server starts with every slot free and re-admits running
// chats as their runners re-acquire, so slot leaks cannot survive a
// crash.
type agentLimiter struct {
	entitlements    *entitlements.Set
	sem             *semaphore.Weighted
	capacity        int64
	clock           quartz.Clock
	logger          slog.Logger
	recheckInterval time.Duration

	slotsInUse prometheus.Gauge
	slotWaits  prometheus.Counter
}

func newAgentLimiter(opts agentLimiterOptions) *agentLimiter {
	if opts.Entitlements == nil {
		opts.Entitlements = entitlements.New()
	}
	if opts.Clock == nil {
		opts.Clock = quartz.NewReal()
	}
	if opts.Capacity <= 0 {
		opts.Capacity = MaxConcurrentAgents
	}
	if opts.EntitlementRecheckInterval <= 0 {
		opts.EntitlementRecheckInterval = defaultEntitlementRecheckInterval
	}
	slotsInUse := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "coderd",
		Subsystem: "chatd",
		Name:      "agent_slots_in_use",
		Help:      "Number of concurrent-agent slots held by executing chat turns. Stays zero on deployments entitled to unlimited chat agents.",
	})
	slotWaits := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "coderd",
		Subsystem: "chatd",
		Name:      "agent_slot_waits_total",
		Help:      "Total number of times a chat turn blocked waiting for a concurrent-agent slot.",
	})
	if opts.Registerer != nil {
		opts.Registerer.MustRegister(slotsInUse, slotWaits)
	}
	return &agentLimiter{
		entitlements:    opts.Entitlements,
		sem:             semaphore.NewWeighted(opts.Capacity),
		capacity:        opts.Capacity,
		clock:           opts.Clock,
		logger:          opts.Logger,
		recheckInterval: opts.EntitlementRecheckInterval,
		slotsInUse:      slotsInUse,
		slotWaits:       slotWaits,
	}
}

func (l *agentLimiter) newLease(chatID uuid.UUID) *agentSlotLease {
	return &agentSlotLease{limiter: l, chatID: chatID}
}

func (l *agentLimiter) entitled() bool {
	return l.entitlements.Enabled(codersdk.FeatureUnlimitedChatAgents)
}

// acquireUnit blocks until it acquires a semaphore unit, the deployment
// becomes entitled to unlimited agents, or ctx is canceled. It returns
// true when a unit was acquired and false when the entitlement bypass
// applies. The entitled path is the fast path: one mutex-guarded map
// lookup, no semaphore interaction, no allocation.
func (l *agentLimiter) acquireUnit(ctx context.Context, chatID uuid.UUID) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if l.entitled() {
		return false, nil
	}
	// TryAcquire succeeds only when capacity is free and no waiters are
	// queued, so this non-blocking path cannot overtake blocked chats.
	if l.sem.TryAcquire(1) {
		l.slotsInUse.Inc()
		return true, nil
	}
	l.slotWaits.Inc()
	start := l.clock.Now()
	l.logger.Info(ctx, "chat waiting for a concurrent-agent slot",
		slog.F("chat_id", chatID),
		slog.F("max_concurrent_agents", l.capacity),
	)
	for {
		// Bound each wait so the entitlement bypass is re-evaluated
		// periodically: installing a Premium license unblocks queued
		// chats within the recheck interval.
		waitCtx, cancelWait := context.WithCancel(ctx)
		recheck := l.clock.AfterFunc(l.recheckInterval, cancelWait, "chatd", "agent_slot_recheck")
		err := l.sem.Acquire(waitCtx, 1)
		recheck.Stop()
		cancelWait()
		if err == nil {
			if l.entitled() {
				// Became entitled while queued; return the unit so it
				// cannot leak on the now-bypassed path.
				l.sem.Release(1)
				return false, nil
			}
			l.slotsInUse.Inc()
			l.logger.Info(ctx, "chat acquired a concurrent-agent slot",
				slog.F("chat_id", chatID),
				slog.F("wait_duration", l.clock.Since(start)),
			)
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if l.entitled() {
			return false, nil
		}
	}
}

func (l *agentLimiter) releaseUnit() {
	l.sem.Release(1)
	l.slotsInUse.Dec()
}

// agentSlotLeaseHandle is the narrow lease surface exposed to
// generation-task code (tool handlers, turn finishers) through the task
// context. Runner-lifecycle methods stay off it on purpose.
type agentSlotLeaseHandle interface {
	Pause()
	Resume(ctx context.Context) error
	MarkTurnComplete()
}

type agentSlotLeaseCtxKey struct{}

// agentSlotLeaseFromContext returns the lease injected by the runner
// into generation task contexts. Absent on entitled deployments and for
// non-generation callers; callers treat absence as a no-op.
func agentSlotLeaseFromContext(ctx context.Context) (agentSlotLeaseHandle, bool) {
	lease, ok := ctx.Value(agentSlotLeaseCtxKey{}).(agentSlotLeaseHandle)
	return lease, ok
}

// agentSlotLease tracks one runner's hold on a concurrent-agent slot. A
// runner owns exactly one lease for its chat, and the lease holds at
// most one semaphore unit at a time. The lease outlives the individual
// generation tasks that make up a turn: the first generation task of a
// turn acquires the slot, step boundaries keep it, and the slot is
// released once the turn ends and the last in-flight generation task
// exits. Entitled deployments run with no unit held.
type agentSlotLease struct {
	limiter *agentLimiter
	chatID  uuid.UUID

	mu sync.Mutex
	// closed is terminal; set at runner teardown.
	closed bool
	// holdsUnit reports whether the lease currently holds a semaphore
	// unit.
	holdsUnit bool
	// pauseRefs counts nested Pause calls. Local tool calls run in
	// parallel goroutines, so concurrent wait_agent calls share the
	// parent's slot: the first Pause releases the unit and the last
	// Resume re-acquires it.
	pauseRefs int
	// tasksInFlight counts generation task goroutines between BeginTask
	// and EndTask. Releases requested while a task still executes are
	// deferred to the last EndTask so a slot is never freed while the
	// chat is unwinding provider work.
	tasksInFlight int
	// releaseRequested defers a MarkTurnComplete release until the last
	// in-flight task exits (or the next EnsureHeld, which yields and
	// re-acquires at the back of the waiter queue).
	releaseRequested bool
}

// BeginTask marks a generation task goroutine as executing.
func (l *agentSlotLease) BeginTask() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tasksInFlight++
}

// EndTask marks a generation task goroutine as exited and performs any
// release deferred while it was executing.
func (l *agentSlotLease) EndTask() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tasksInFlight--
	if l.tasksInFlight > 0 {
		return
	}
	// Pauses cannot outlive the tasks whose tool calls created them;
	// clear anything a failed tool goroutine left behind.
	l.pauseRefs = 0
	if l.releaseRequested {
		l.releaseRequested = false
		l.releaseUnitLocked()
	}
}

// EnsureHeld acquires the chat's agent slot unless the lease already
// holds one. Generation tasks call this at each attempt: mid-turn calls
// are no-ops, while the first task of a turn (or an attempt following a
// failed Resume) performs the acquire. A pending MarkTurnComplete
// release (a promoted queued message started a new turn before the
// previous task finished unwinding) is honored first, so the new turn
// queues behind other waiting chats.
func (l *agentSlotLease) EnsureHeld(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errAgentSlotLeaseClosed
	}
	if l.releaseRequested {
		l.releaseRequested = false
		l.releaseUnitLocked()
	}
	if l.holdsUnit {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	return l.acquire(ctx)
}

// acquire obtains a unit (or the entitlement bypass) and installs it on
// the lease. When a concurrent Close, acquire, or Pause won the race,
// the extra unit is returned to the limiter.
func (l *agentSlotLease) acquire(ctx context.Context) error {
	holdsUnit, err := l.limiter.acquireUnit(ctx, l.chatID)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.holdsUnit || l.pauseRefs > 0 {
		if holdsUnit {
			l.limiter.releaseUnit()
		}
		if l.closed {
			return errAgentSlotLeaseClosed
		}
		return nil
	}
	l.holdsUnit = holdsUnit
	return nil
}

// Pause yields the slot while the holder blocks on external completion
// (wait_agent), freeing capacity for subagent children. Reference
// counted: only the first Pause releases the unit.
func (l *agentSlotLease) Pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.pauseRefs++
	if l.pauseRefs == 1 {
		l.releaseUnitLocked()
	}
}

// Resume undoes one Pause; the last Resume re-acquires the slot,
// blocking until one frees. A canceled Resume leaves the lease unheld
// and returns the context error; the next generation attempt re-acquires
// through EnsureHeld.
func (l *agentSlotLease) Resume(ctx context.Context) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errAgentSlotLeaseClosed
	}
	if l.pauseRefs > 0 {
		l.pauseRefs--
	}
	last := l.pauseRefs == 0
	l.mu.Unlock()
	if !last {
		return nil
	}
	return l.acquire(ctx)
}

// MarkTurnComplete flags the current turn's slot hold for release. The
// release happens immediately when no generation task is executing, and
// otherwise when the last in-flight task exits. Called by turn
// finishers (FinishTurn, FinishError, EnterRequiresAction) and by the
// runner when the chat leaves running status (interrupt, archive).
func (l *agentSlotLease) MarkTurnComplete() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if l.tasksInFlight > 0 {
		l.releaseRequested = true
		return
	}
	l.releaseUnitLocked()
}

// Close releases any held unit and permanently invalidates the lease.
// Idempotent; the runner calls it unconditionally at teardown. Acquires
// racing with Close return their unit immediately.
func (l *agentSlotLease) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.releaseRequested = false
	l.releaseUnitLocked()
}

// attachToContext injects the lease into a generation task context so
// wait_agent can pause it and turn finishers can mark it complete.
// Leases holding no unit (entitled deployments) are not injected: every
// handle method would be a no-op.
func (l *agentSlotLease) attachToContext(ctx context.Context) context.Context {
	l.mu.Lock()
	holdsUnit := l.holdsUnit
	l.mu.Unlock()
	if !holdsUnit {
		return ctx
	}
	return context.WithValue(ctx, agentSlotLeaseCtxKey{}, agentSlotLeaseHandle(l))
}

func (l *agentSlotLease) releaseUnitLocked() {
	if !l.holdsUnit {
		return
	}
	l.holdsUnit = false
	l.limiter.releaseUnit()
}
