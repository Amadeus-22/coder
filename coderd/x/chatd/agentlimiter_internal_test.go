package chatd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/entitlements"
	"github.com/coder/coder/v2/coderd/x/chatd/chatstate"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
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

func leaseHoldsUnit(l *agentSlotLease) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdsUnit
}

func TestAgentLimiter_EntitledBypass(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, unlimitedChatAgentsEntitlements())

	// Far more leases than capacity acquire without blocking.
	for range 5 {
		lease := limiter.newLease(uuid.New())
		require.NoError(t, lease.EnsureHeld(ctx))
		require.False(t, leaseHoldsUnit(lease))
		// Entitled leases are not injected into task contexts and do
		// no metrics work.
		require.Equal(t, ctx, lease.attachToContext(ctx)) //nolint:revive // identity check, not ctx misuse
	}
	require.Zero(t, promtestutil.ToFloat64(limiter.slotsInUse))
	require.Zero(t, promtestutil.ToFloat64(limiter.slotWaits))
}

func TestAgentLimiter_MidWaitEntitlementInstall(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	ents := entitlements.New()
	limiter := newTestAgentLimiter(t, 1, ents)

	holder := limiter.newLease(uuid.New())
	require.NoError(t, holder.EnsureHeld(ctx))

	waiter := limiter.newLease(uuid.New())
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

	holder := limiter.newLease(uuid.New())
	require.NoError(t, holder.EnsureHeld(ctx))

	waitCtx, cancelWait := context.WithCancel(ctx)
	waiter := limiter.newLease(uuid.New())
	acquired := make(chan error, 1)
	go func() { acquired <- waiter.EnsureHeld(waitCtx) }()
	cancelWait()
	require.ErrorIs(t, testutil.RequireReceive(ctx, t, acquired), context.Canceled)
	require.False(t, leaseHoldsUnit(waiter))

	// The canceled wait must not leak capacity: once the holder
	// releases, a fresh lease acquires immediately.
	holder.MarkTurnComplete()
	require.False(t, leaseHoldsUnit(holder))
	next := limiter.newLease(uuid.New())
	require.NoError(t, next.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(next))
}

func TestAgentSlotLease_PauseResumeRefCount(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	parent := limiter.newLease(uuid.New())
	require.NoError(t, parent.EnsureHeld(ctx))

	// Two parallel wait_agent calls share the parent's slot: the first
	// Pause releases the unit.
	parent.Pause()
	parent.Pause()
	require.False(t, leaseHoldsUnit(parent))

	// The freed slot is usable by another chat (for example a child).
	child := limiter.newLease(uuid.New())
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

	parent := limiter.newLease(uuid.New())
	require.NoError(t, parent.EnsureHeld(ctx))
	parent.Pause()

	other := limiter.newLease(uuid.New())
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

	lease := limiter.newLease(uuid.New())
	require.NoError(t, lease.EnsureHeld(ctx))
	require.Equal(t, float64(1), promtestutil.ToFloat64(limiter.slotsInUse))

	lease.Close()
	lease.Close() // idempotent
	require.False(t, leaseHoldsUnit(lease))
	require.Zero(t, promtestutil.ToFloat64(limiter.slotsInUse))
	require.ErrorIs(t, lease.EnsureHeld(ctx), errAgentSlotLeaseClosed)
	require.ErrorIs(t, lease.Resume(ctx), errAgentSlotLeaseClosed)

	// The closed lease returned its unit.
	next := limiter.newLease(uuid.New())
	require.NoError(t, next.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(next))
}

func TestAgentSlotLease_TurnCompleteDeferredUntilTaskExit(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	lease := limiter.newLease(uuid.New())
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

func TestAgentSlotLease_EnsureHeldYieldsPendingRelease(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)
	limiter := newTestAgentLimiter(t, 1, nil)

	promoted := limiter.newLease(uuid.New())
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
	other := limiter.newLease(uuid.New())
	require.NoError(t, other.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(other))

	// The promoted turn re-acquires once the slot frees, completing the
	// no-starvation handoff.
	other.MarkTurnComplete()
	require.NoError(t, promoted.EnsureHeld(ctx))
	require.True(t, leaseHoldsUnit(promoted))
}

// testAgentLimiterOptions returns worker options with a capacity-1
// limiter so slot handover is deterministic in tests.
func testAgentLimiterOptions(t *testing.T, f *workerTestFixture, starter chatWorkerTaskStarter, ents *entitlements.Set) chatWorkerOptions {
	t.Helper()
	opts := testOptions(t, f, starter)
	opts.AgentLimiter = newAgentLimiter(agentLimiterOptions{
		Entitlements:               ents,
		Logger:                     testutil.Logger(t),
		Capacity:                   1,
		EntitlementRecheckInterval: 10 * time.Millisecond,
	})
	return opts
}

func waitTaskCalls(t *testing.T, starter *recordingTaskStarter, n int) []taskCall {
	t.Helper()
	calls := make([]taskCall, 0, n)
	deadline := time.After(testutil.WaitLong)
	for len(calls) < n {
		select {
		case call := <-starter.callCh:
			calls = append(calls, call)
		case <-deadline:
			t.Fatalf("timed out waiting for %d task calls, got %d", n, len(calls))
		}
	}
	return calls
}

func assertNoGenerationCall(t *testing.T, starter *recordingTaskStarter) {
	t.Helper()
	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case call := <-starter.callCh:
			if call.kind == taskKindGeneration {
				t.Fatalf("unexpected generation call for chat %s", call.input.ChatID)
			}
		case <-timeout:
			return
		}
	}
}

func TestWorker_AgentLimiterCapsConcurrentGenerations(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	// The default limiter caps at MaxConcurrentAgents with no
	// entitlements.
	opts := testOptions(t, f, starter)

	// More chats than slots are already running before the worker
	// starts, as after a coderd restart mid-turn.
	total := MaxConcurrentAgents + 2
	for range total {
		f.createRunningChat(t)
	}
	startWorker(t, opts)

	started := make(map[uuid.UUID]bool)
	for range MaxConcurrentAgents {
		call := starter.waitCall(t, taskKindGeneration, uuid.Nil)
		started[call.input.ChatID] = true
	}
	require.Len(t, started, MaxConcurrentAgents)
	// The remaining chats stay queued.
	starter.assertNoCall(t)

	// Finishing one turn admits exactly one queued chat.
	var finished uuid.UUID
	for id := range started {
		finished = id
		break
	}
	finishTurn(t, f, finished)
	call := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.False(t, started[call.input.ChatID], "a queued chat should be admitted, not a running one")
	assertNoGenerationCall(t, starter)
}

func TestWorker_AgentLimiterHoldsSlotAcrossSteps(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts := testAgentLimiterOptions(t, f, starter, nil)

	f.createRunningChat(t)
	f.createRunningChat(t)
	startWorker(t, opts)

	first := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	holder := first.input.ChatID
	starter.assertNoCall(t)

	// A step commit replaces the generation task within the same turn;
	// the slot is kept, so the other chat stays queued.
	commitAssistantStep(t, f, holder, "step")
	second := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.Equal(t, holder, second.input.ChatID)
	assertNoGenerationCall(t, starter)

	// Finishing the turn frees the slot for the queued chat.
	finishTurn(t, f, holder)
	third := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.NotEqual(t, holder, third.input.ChatID)
}

func TestWorker_AgentLimiterInterruptBypassesAndFreesSlot(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts := testAgentLimiterOptions(t, f, starter, nil)

	chatA := f.createRunningChat(t)
	chatB := f.createRunningChat(t)
	startWorker(t, opts)

	first := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	holder := first.input.ChatID
	queued := chatA.ID
	if holder == chatA.ID {
		queued = chatB.ID
	}
	starter.assertNoCall(t)

	// Interrupting the slot holder starts its interrupt task without
	// waiting for a slot and hands the freed slot to the queued chat.
	interruptChat(t, f, holder)
	calls := waitTaskCalls(t, starter, 2)
	kinds := make(map[taskKind]uuid.UUID, 2)
	for _, call := range calls {
		kinds[call.kind] = call.input.ChatID
	}
	require.Equal(t, holder, kinds[taskKindInterrupt])
	require.Equal(t, queued, kinds[taskKindGeneration])
}

func TestWorker_AgentLimiterRequiresActionReleasesAndReacquires(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts := testAgentLimiterOptions(t, f, starter, nil)

	chatA := f.createRunningChat(t)
	chatB := f.createRunningChat(t)
	startWorker(t, opts)

	first := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	holder := first.input.ChatID
	queued := chatA.ID
	if holder == chatA.ID {
		queued = chatB.ID
	}
	starter.assertNoCall(t)

	// An external tool wait releases the slot; the queued chat runs.
	forceExecutionStateAndPublish(t, f, holder, database.ChatStatusRequiresAction, false)
	second := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.Equal(t, queued, second.input.ChatID)

	// Resuming the requires-action chat queues it for a slot again; it
	// runs once the current holder finishes.
	forceExecutionStateAndPublish(t, f, holder, database.ChatStatusRunning, false)
	assertNoGenerationCall(t, starter)
	finishTurn(t, f, queued)
	third := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.Equal(t, holder, third.input.ChatID)
}

func TestWorker_AgentLimiterEntitledUncapped(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts := testAgentLimiterOptions(t, f, starter, unlimitedChatAgentsEntitlements())

	chatA := f.createRunningChat(t)
	chatB := f.createRunningChat(t)
	startWorker(t, opts)

	// Both chats generate concurrently despite the capacity-1 limiter.
	calls := waitTaskCalls(t, starter, 2)
	got := make(map[uuid.UUID]bool, 2)
	for _, call := range calls {
		require.Equal(t, taskKindGeneration, call.kind)
		got[call.input.ChatID] = true
	}
	require.True(t, got[chatA.ID])
	require.True(t, got[chatB.ID])
}

// agentSlotPausingStarter simulates a parent chat blocked in wait_agent:
// its generation pauses the injected lease, waits for the test's signal,
// then resumes.
type agentSlotPausingStarter struct {
	*recordingTaskStarter
	pauseChatID uuid.UUID
	paused      chan struct{}
	resume      chan struct{}
	resumeErr   chan error
}

func newAgentSlotPausingStarter(pauseChatID uuid.UUID) *agentSlotPausingStarter {
	return &agentSlotPausingStarter{
		recordingTaskStarter: newRecordingTaskStarter(),
		pauseChatID:          pauseChatID,
		paused:               make(chan struct{}, 1),
		resume:               make(chan struct{}),
		resumeErr:            make(chan error, 1),
	}
}

func (s *agentSlotPausingStarter) StartGeneration(ctx context.Context, input chatWorkerTaskStartInput) error {
	if input.ChatID != s.pauseChatID {
		return s.recordingTaskStarter.StartGeneration(ctx, input)
	}
	lease, ok := agentSlotLeaseFromContext(ctx)
	if !ok {
		return errors.Join(errTaskExpectedExit, xerrors.New("no agent slot lease in generation context"))
	}
	lease.Pause()
	s.paused <- struct{}{}
	select {
	case <-s.resume:
	case <-ctx.Done():
		return errors.Join(errTaskExpectedExit, ctx.Err())
	}
	err := lease.Resume(ctx)
	s.resumeErr <- err
	return err
}

// TestWorker_AgentLimiterSubagentPauseAvoidsDeadlock is the deadlock
// regression for spawn_agent + wait_agent at one slot: the parent yields
// while waiting, the child runs to completion, and the parent then
// re-acquires.
func TestWorker_AgentLimiterSubagentPauseAvoidsDeadlock(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newWorkerTestFixture(t)

	parent := f.createRunningChat(t)
	starter := newAgentSlotPausingStarter(parent.ID)
	opts := testAgentLimiterOptions(t, f, starter, nil)
	worker := startWorker(t, opts)

	// The parent takes the only slot and yields it, as wait_agent does.
	testutil.RequireReceive(ctx, t, starter.paused)

	// The child can now run to completion on the freed slot.
	child := f.createRunningChat(t)
	worker.Wake()
	starter.waitCall(t, taskKindGeneration, child.ID)

	// The parent's resume queues behind the child's running turn.
	close(starter.resume)
	finishTurn(t, f, child.ID)
	require.NoError(t, testutil.RequireReceive(ctx, t, starter.resumeErr))
}

func createRunningChildChat(t *testing.T, f *workerTestFixture, parentID uuid.UUID) database.Chat {
	t.Helper()
	ctx := testutil.Context(t, testutil.WaitShort)
	res, err := chatstate.CreateChat(ctx, f.db, f.pubsub, chatstate.CreateChatInput{
		OrganizationID:    f.org.ID,
		OwnerID:           f.user.ID,
		LastModelConfigID: f.model.ID,
		Title:             "child",
		ClientType:        database.ChatClientTypeApi,
		ParentChatID:      uuid.NullUUID{UUID: parentID, Valid: true},
		InitialMessages: []chatstate.Message{
			userTextMessage(t, "hello", f.user.ID, f.model.ID, f.apiKey.ID),
		},
	})
	require.NoError(t, err)
	return res.Chat
}

// TestAwaitSubagentCompletion_PausesAgentSlot exercises the wait_agent
// integration directly: the parent's slot is yielded while awaiting a
// child and re-acquired before returning.
func TestAwaitSubagentCompletion_PausesAgentSlot(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newWorkerTestFixture(t)
	server := &Server{
		db:     f.db,
		pubsub: f.pubsub,
		clock:  quartz.NewReal(),
		logger: testutil.Logger(t),
	}

	parent := f.createRunningChat(t)
	child := createRunningChildChat(t, f, parent.ID)

	limiter := newTestAgentLimiter(t, 1, nil)
	lease := limiter.newLease(parent.ID)
	require.NoError(t, lease.EnsureHeld(ctx))
	taskCtx := lease.attachToContext(ctx)

	done := make(chan error, 1)
	go func() {
		_, _, err := server.awaitSubagentCompletion(taskCtx, parent.ID, child.ID, testutil.WaitLong)
		done <- err
	}()

	// The parent yields its slot while waiting, so another chat can
	// acquire it. This blocks until the pause happens.
	other := limiter.newLease(uuid.New())
	require.NoError(t, other.EnsureHeld(ctx))

	// The child completes, but the parent cannot resume until the slot
	// frees again.
	finishTurn(t, f, child.ID)
	select {
	case err := <-done:
		t.Fatalf("await returned before the slot was re-acquired: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	other.MarkTurnComplete()
	require.NoError(t, testutil.RequireReceive(ctx, t, done))
	require.True(t, leaseHoldsUnit(lease))
}
