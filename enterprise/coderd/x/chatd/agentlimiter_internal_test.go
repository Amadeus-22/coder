package chatd

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/coderd/entitlements"
	osschatd "github.com/coder/coder/v2/coderd/x/chatd"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
)

func newTestAgentLimiter(t *testing.T, capacity int64, ents *entitlements.Set) *agentLimiter {
	t.Helper()
	return newAgentLimiter(agentLimiterOptions{
		Entitlements:               ents,
		Logger:                     testutil.Logger(t),
		Capacity:                   capacity,
		EntitlementRecheckInterval: 10 * time.Millisecond,
	})
}

func unlimitedChatAgentsEntitlements() *entitlements.Set {
	set := entitlements.New()
	enableUnlimitedChatAgents(set)
	return set
}

func enableUnlimitedChatAgents(set *entitlements.Set) {
	set.Modify(func(entitlements *codersdk.Entitlements) {
		entitlements.Features[codersdk.FeatureUnlimitedChatAgents] = codersdk.Feature{
			Entitlement: codersdk.EntitlementEntitled,
			Enabled:     true,
		}
	})
}

// leaseForTest returns the concrete lease type so tests can inspect it.
func (l *agentLimiter) leaseForTest(chatID uuid.UUID) *agentSlotLease {
	lease, _ := l.NewLease(chatID).(*agentSlotLease)
	return lease
}

func leaseHoldsUnit(l *agentSlotLease) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsUnit
}

func TestAgentLimiterFactory(t *testing.T) {
	t.Parallel()

	t.Run("Unlicensed", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitShort)
		factory := NewAgentLimiterFactory(entitlements.New())
		lease := factory(agentLimiterFactoryOptions(t)).NewLease(uuid.New())
		require.NoError(t, lease.EnsureHeld(ctx))
		// The cap applies: the lease holds a slot and is injected into
		// task contexts.
		require.NotEqual(t, ctx, lease.AttachToContext(ctx)) //nolint:revive // identity check, not ctx misuse
	})

	t.Run("Entitled", func(t *testing.T) {
		t.Parallel()
		ctx := testutil.Context(t, testutil.WaitShort)
		factory := NewAgentLimiterFactory(unlimitedChatAgentsEntitlements())
		lease := factory(agentLimiterFactoryOptions(t)).NewLease(uuid.New())
		require.NoError(t, lease.EnsureHeld(ctx))
		require.Equal(t, ctx, lease.AttachToContext(ctx)) //nolint:revive // identity check, not ctx misuse
	})
}

func agentLimiterFactoryOptions(t *testing.T) osschatd.AgentLimiterOptions {
	t.Helper()
	return osschatd.AgentLimiterOptions{Logger: testutil.Logger(t)}
}

func TestAgentLimiter_EntitledBypass(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, unlimitedChatAgentsEntitlements())

	// Far more leases than capacity acquire without blocking.
	for range 5 {
		lease := limiter.leaseForTest(uuid.New())
		require.NoError(t, lease.EnsureHeld(ctx))
		require.False(t, leaseHoldsUnit(lease))
		// Entitled leases are not injected into task contexts and do
		// no metrics work.
		require.Equal(t, ctx, lease.AttachToContext(ctx)) //nolint:revive // identity check, not ctx misuse
	}
	require.Zero(t, promtestutil.ToFloat64(limiter.slotsInUse))
	require.Zero(t, promtestutil.ToFloat64(limiter.slotWaits))
}

func TestAgentLimiter_MidWaitEntitlementInstall(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	ents := entitlements.New()
	limiter := newTestAgentLimiter(t, 1, ents)

	holder := limiter.leaseForTest(uuid.New())
	require.NoError(t, holder.EnsureHeld(ctx))

	waiter := limiter.leaseForTest(uuid.New())
	acquired := make(chan error, 1)
	go func() { acquired <- waiter.EnsureHeld(ctx) }()

	// The waiter blocks: the only slot is held.
	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(limiter.slotWaits) >= 1
	}, testutil.WaitShort, testutil.IntervalFast)

	// Installing a Premium license unblocks the waiter within the
	// recheck interval, without the holder freeing its slot.
	enableUnlimitedChatAgents(ents)
	require.NoError(t, testutil.RequireReceive(ctx, t, acquired))
	require.False(t, leaseHoldsUnit(waiter))
	require.True(t, leaseHoldsUnit(holder))
}

func TestAgentLimiter_CancelWhileWaiting(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	holder := limiter.leaseForTest(uuid.New())
	require.NoError(t, holder.EnsureHeld(ctx))

	waitCtx, cancelWait := context.WithCancel(ctx)
	waiter := limiter.leaseForTest(uuid.New())
	acquired := make(chan error, 1)
	go func() { acquired <- waiter.EnsureHeld(waitCtx) }()
	cancelWait()
	require.ErrorIs(t, testutil.RequireReceive(ctx, t, acquired), context.Canceled)
	require.False(t, leaseHoldsUnit(waiter))

	// The canceled wait must not leak capacity: once the holder
	// releases, a fresh lease acquires immediately.
	holder.MarkTurnComplete()
	require.False(t, leaseHoldsUnit(holder))
	next := limiter.leaseForTest(uuid.New())
	require.NoError(t, next.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(next))
}

func TestAgentSlotLease_PauseResumeRefCount(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	parent := limiter.leaseForTest(uuid.New())
	require.NoError(t, parent.EnsureHeld(ctx))

	// Two parallel wait_agent calls share the parent's slot: the first
	// Pause releases the unit.
	parent.Pause()
	parent.Pause()
	require.False(t, leaseHoldsUnit(parent))

	// The freed slot is usable by another chat (for example a child).
	child := limiter.leaseForTest(uuid.New())
	require.NoError(t, child.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(child))
	child.MarkTurnComplete()

	// The first Resume keeps the lease paused; the last re-acquires.
	require.NoError(t, parent.Resume(ctx))
	require.False(t, leaseHoldsUnit(parent))
	require.NoError(t, parent.Resume(ctx))
	require.True(t, leaseHoldsUnit(parent))
}

func TestAgentSlotLease_ResumeCanceled(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	parent := limiter.leaseForTest(uuid.New())
	require.NoError(t, parent.EnsureHeld(ctx))
	parent.Pause()

	other := limiter.leaseForTest(uuid.New())
	require.NoError(t, other.EnsureHeld(ctx))

	// Resume with a canceled context leaves the lease unheld without
	// corrupting accounting; the next EnsureHeld re-acquires.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, parent.Resume(canceledCtx), context.Canceled)
	require.False(t, leaseHoldsUnit(parent))

	other.MarkTurnComplete()
	require.NoError(t, parent.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(parent))
}

func TestAgentSlotLease_Close(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	lease := limiter.leaseForTest(uuid.New())
	require.NoError(t, lease.EnsureHeld(ctx))
	require.Equal(t, float64(1), promtestutil.ToFloat64(limiter.slotsInUse))

	lease.Close()
	lease.Close() // idempotent
	require.False(t, leaseHoldsUnit(lease))
	require.Zero(t, promtestutil.ToFloat64(limiter.slotsInUse))
	require.ErrorIs(t, lease.EnsureHeld(ctx), errAgentSlotLeaseClosed)
	require.ErrorIs(t, lease.Resume(ctx), errAgentSlotLeaseClosed)

	// The closed lease returned its unit.
	next := limiter.leaseForTest(uuid.New())
	require.NoError(t, next.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(next))
}

func TestAgentSlotLease_TurnCompleteDeferredUntilTaskExit(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	lease := limiter.leaseForTest(uuid.New())
	lease.BeginTask()
	require.NoError(t, lease.EnsureHeld(ctx))
	lease.MarkTurnComplete()
	// The slot is not freed while the generation task is unwinding.
	require.True(t, leaseHoldsUnit(lease))
	require.Equal(t, float64(1), promtestutil.ToFloat64(limiter.slotsInUse))
	lease.EndTask()
	require.False(t, leaseHoldsUnit(lease))
	require.Zero(t, promtestutil.ToFloat64(limiter.slotsInUse))
}

func TestAgentSlotLease_ReacquireKeepsPendingRelease(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	lease := limiter.leaseForTest(uuid.New())
	lease.BeginTask()
	require.NoError(t, lease.EnsureHeld(ctx))
	lease.MarkTurnComplete()

	// A doomed retry keeps the slot instead of yielding and blocking on
	// a re-queue it would only exit the task fence with.
	require.NoError(t, lease.Reacquire(ctx))
	require.True(t, leaseHoldsUnit(lease))

	// The pending release still happens at task exit.
	lease.EndTask()
	require.False(t, leaseHoldsUnit(lease))
}

func TestAgentSlotLease_ReacquireAfterFailedResume(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	lease := limiter.leaseForTest(uuid.New())
	require.NoError(t, lease.EnsureHeld(ctx))
	lease.Pause()

	// A failed Resume leaves the lease unheld; Reacquire recovers it.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, lease.Resume(canceledCtx), context.Canceled)
	require.False(t, leaseHoldsUnit(lease))
	require.NoError(t, lease.Reacquire(ctx))
	require.True(t, leaseHoldsUnit(lease))

	lease.Close()
	require.ErrorIs(t, lease.Reacquire(ctx), errAgentSlotLeaseClosed)
}

func TestAgentSlotLease_EnsureHeldYieldsPendingRelease(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	promoted := limiter.leaseForTest(uuid.New())
	promoted.BeginTask()
	require.NoError(t, promoted.EnsureHeld(ctx))
	// The turn finished with a promoted queued message: the release is
	// pending while the finished turn's task is still unwinding.
	promoted.MarkTurnComplete()
	require.True(t, leaseHoldsUnit(promoted))

	// EnsureHeld honors the pending release before re-acquiring: even
	// when the re-acquire is aborted (canceled context), the yield has
	// already happened. Semaphore admission order is not strict FIFO,
	// so this verifies release-before-reacquire without racing a
	// concurrent waiter.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, promoted.EnsureHeld(canceledCtx), context.Canceled)
	require.False(t, leaseHoldsUnit(promoted))

	// The yielded slot is immediately available to another chat.
	other := limiter.leaseForTest(uuid.New())
	require.NoError(t, other.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(other))

	// The promoted turn re-acquires once the slot frees, completing the
	// no-starvation handoff.
	other.MarkTurnComplete()
	require.NoError(t, promoted.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(promoted))
}
