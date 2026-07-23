package chatd

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/coderd/database"
	coderdpubsub "github.com/coder/coder/v2/coderd/pubsub"
	"github.com/coder/coder/v2/coderd/x/chatd/chatstate"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/quartz"
)

// fakeAgentLimiter hands out recording leases whose EnsureHeld blocks
// until the test admits the chat. It has no capacity logic on purpose:
// these tests pin the worker's side of the AgentLimiter contract, while
// enforcement semantics are tested against the real limiter in
// enterprise/coderd/x/chatd.
type fakeAgentLimiter struct {
	mu     sync.Mutex
	leases map[uuid.UUID]*fakeAgentSlotLease
}

func newFakeAgentLimiter() *fakeAgentLimiter {
	return &fakeAgentLimiter{leases: make(map[uuid.UUID]*fakeAgentSlotLease)}
}

func (f *fakeAgentLimiter) NewLease(chatID uuid.UUID) AgentSlotLease {
	return f.lease(chatID)
}

func (f *fakeAgentLimiter) lease(chatID uuid.UUID) *fakeAgentSlotLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	if lease, ok := f.leases[chatID]; ok {
		return lease
	}
	lease := newFakeAgentSlotLease()
	f.leases[chatID] = lease
	return lease
}

// admit unblocks all current and future EnsureHeld calls for chatID.
func (f *fakeAgentLimiter) admit(chatID uuid.UUID) {
	f.lease(chatID).admit()
}

type fakeAgentSlotLease struct {
	admitCh chan struct{}

	mu        sync.Mutex
	admitted  bool
	events    []string
	resumeErr error
}

func newFakeAgentSlotLease() *fakeAgentSlotLease {
	return &fakeAgentSlotLease{admitCh: make(chan struct{})}
}

func (l *fakeAgentSlotLease) admit() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.admitted {
		return
	}
	l.admitted = true
	close(l.admitCh)
}

func (l *fakeAgentSlotLease) record(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *fakeAgentSlotLease) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func (l *fakeAgentSlotLease) waitForEvent(t *testing.T, event string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return slices.Contains(l.snapshot(), event)
	}, testutil.WaitLong, testutil.IntervalFast, "expected lease event %q, got %v", event, l.snapshot())
}

func (l *fakeAgentSlotLease) BeginTask() { l.record("begin_task") }
func (l *fakeAgentSlotLease) EndTask()   { l.record("end_task") }

func (l *fakeAgentSlotLease) EnsureHeld(ctx context.Context) error {
	l.record("ensure_held")
	select {
	case <-l.admitCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reacquire never blocks: the fake's admission gate models turn
// admission (EnsureHeld), not the lost-slot recovery path.
func (l *fakeAgentSlotLease) Reacquire(context.Context) error {
	l.record("reacquire")
	return nil
}

func (l *fakeAgentSlotLease) MarkTurnComplete() { l.record("turn_complete") }
func (l *fakeAgentSlotLease) Close()            { l.record("close") }

func (l *fakeAgentSlotLease) AttachToContext(ctx context.Context) context.Context {
	return WithAgentSlotLease(ctx, l)
}

func (l *fakeAgentSlotLease) Pause() { l.record("pause") }

func (l *fakeAgentSlotLease) Resume(context.Context) error {
	l.record("resume")
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resumeErr
}

func testFakeLimiterOptions(t *testing.T, f *workerTestFixture, starter chatWorkerTaskStarter) (chatWorkerOptions, *fakeAgentLimiter) {
	t.Helper()
	opts := testOptions(t, f, starter)
	limiter := newFakeAgentLimiter()
	opts.AgentLimiter = limiter
	return opts, limiter
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

// TestWorker_AgentLimiterGatesGeneration pins the admission contract:
// every generation task calls EnsureHeld before StartGeneration runs,
// and a blocked EnsureHeld keeps that chat queued without affecting
// other chats.
func TestWorker_AgentLimiterGatesGeneration(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts, limiter := testFakeLimiterOptions(t, f, starter)

	chatA := f.createRunningChat(t)
	chatB := f.createRunningChat(t)
	startWorker(t, opts)

	// Both runners request a slot; neither generation starts.
	limiter.lease(chatA.ID).waitForEvent(t, "ensure_held")
	limiter.lease(chatB.ID).waitForEvent(t, "ensure_held")
	starter.assertNoCall(t)

	// Admission is per chat.
	limiter.admit(chatA.ID)
	call := starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.Equal(t, chatA.ID, call.input.ChatID)
	starter.assertNoCall(t)

	limiter.admit(chatB.ID)
	call = starter.waitCall(t, taskKindGeneration, uuid.Nil)
	require.Equal(t, chatB.ID, call.input.ChatID)
}

// TestWorker_AgentLimiterInterruptDoesNotWaitForSlot pins that
// non-generation tasks bypass the limiter and that leaving the running
// state marks the turn complete.
func TestWorker_AgentLimiterInterruptDoesNotWaitForSlot(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts, limiter := testFakeLimiterOptions(t, f, starter)

	chat := f.createRunningChat(t)
	startWorker(t, opts)
	limiter.lease(chat.ID).waitForEvent(t, "ensure_held")

	// The interrupt task starts while the generation is still blocked
	// waiting for admission.
	interruptChat(t, f, chat.ID)
	call := starter.waitCall(t, taskKindInterrupt, chat.ID)
	require.Equal(t, chat.ID, call.input.ChatID)
	limiter.lease(chat.ID).waitForEvent(t, "turn_complete")
	starter.assertNoCall(t)
}

// TestWorker_AgentLimiterRequiresActionReleasesAndReacquires pins the
// turn-boundary contract for external tool waits: entering
// requires_action marks the turn complete and resuming the chat
// requests a slot again.
func TestWorker_AgentLimiterRequiresActionReleasesAndReacquires(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts, limiter := testFakeLimiterOptions(t, f, starter)

	chat := f.createRunningChat(t)
	limiter.admit(chat.ID)
	startWorker(t, opts)
	starter.waitCall(t, taskKindGeneration, chat.ID)

	forceExecutionStateAndPublish(t, f, chat.ID, database.ChatStatusRequiresAction, false)
	limiter.lease(chat.ID).waitForEvent(t, "turn_complete")

	// Returning to running starts a new turn, which acquires again.
	forceExecutionStateAndPublish(t, f, chat.ID, database.ChatStatusRunning, false)
	starter.waitCall(t, taskKindGeneration, chat.ID)
	events := limiter.lease(chat.ID).snapshot()
	require.GreaterOrEqual(t, countEvents(events, "ensure_held"), 2)
}

func countEvents(events []string, event string) int {
	count := 0
	for _, e := range events {
		if e == event {
			count++
		}
	}
	return count
}

// TestWorker_AgentLimiterClosesLeaseOnShutdown pins that runner
// teardown closes the lease unconditionally.
func TestWorker_AgentLimiterClosesLeaseOnShutdown(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts, limiter := testFakeLimiterOptions(t, f, starter)

	chat := f.createRunningChat(t)
	limiter.admit(chat.ID)
	worker := startWorker(t, opts)
	starter.waitCall(t, taskKindGeneration, chat.ID)

	require.NoError(t, worker.Close())
	limiter.lease(chat.ID).waitForEvent(t, "close")
}

// failOnceStarter fails the first StartGeneration with a retryable
// error and records subsequent calls normally.
type failOnceStarter struct {
	*recordingTaskStarter
	mu     sync.Mutex
	failed bool
}

func (s *failOnceStarter) StartGeneration(ctx context.Context, input chatWorkerTaskStartInput) error {
	s.mu.Lock()
	first := !s.failed
	s.failed = true
	s.mu.Unlock()
	if first {
		return xerrors.New("transient generation failure")
	}
	return s.recordingTaskStarter.StartGeneration(ctx, input)
}

// TestWorker_AgentLimiterRetryReacquires pins that generation retries
// re-check the slot through Reacquire rather than EnsureHeld, so a
// retry never yields a pending turn-complete release back to the
// waiter queue.
func TestWorker_AgentLimiterRetryReacquires(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := &failOnceStarter{recordingTaskStarter: newRecordingTaskStarter()}
	opts, limiter := testFakeLimiterOptions(t, f, starter)
	opts.TaskRetryInitialBackoff = time.Millisecond

	chat := f.createRunningChat(t)
	limiter.admit(chat.ID)
	startWorker(t, opts)

	starter.waitCall(t, taskKindGeneration, chat.ID)
	events := limiter.lease(chat.ID).snapshot()
	require.Equal(t, 1, countEvents(events, "ensure_held"))
	require.GreaterOrEqual(t, countEvents(events, "reacquire"), 2)
}

// leaseHandleStarter simulates a wait_agent tool call: its generation
// pulls the lease handle from the task context, pauses, and resumes.
type leaseHandleStarter struct {
	*recordingTaskStarter
	handleOK  chan bool
	resumeErr chan error
}

func newLeaseHandleStarter() *leaseHandleStarter {
	return &leaseHandleStarter{
		recordingTaskStarter: newRecordingTaskStarter(),
		handleOK:             make(chan bool, 1),
		resumeErr:            make(chan error, 1),
	}
}

func (s *leaseHandleStarter) StartGeneration(ctx context.Context, input chatWorkerTaskStartInput) error {
	handle, ok := agentSlotLeaseFromContext(ctx)
	s.handleOK <- ok
	if !ok {
		return errors.Join(errTaskExpectedExit, xerrors.New("no agent slot lease in generation context"))
	}
	handle.Pause()
	s.resumeErr <- handle.Resume(ctx)
	return s.recordingTaskStarter.StartGeneration(ctx, input)
}

// TestWorker_AgentLimiterAttachesLeaseToGenerationContext pins that the
// runner injects the lease handle into generation task contexts and
// that Pause/Resume round-trip through it.
func TestWorker_AgentLimiterAttachesLeaseToGenerationContext(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newWorkerTestFixture(t)
	starter := newLeaseHandleStarter()
	opts, limiter := testFakeLimiterOptions(t, f, starter)

	chat := f.createRunningChat(t)
	limiter.admit(chat.ID)
	startWorker(t, opts)

	require.True(t, testutil.RequireReceive(ctx, t, starter.handleOK))
	require.NoError(t, testutil.RequireReceive(ctx, t, starter.resumeErr))
	events := limiter.lease(chat.ID).snapshot()
	pause := slices.Index(events, "pause")
	resume := slices.Index(events, "resume")
	require.GreaterOrEqual(t, pause, 0)
	require.Greater(t, resume, pause)
}

// TestWorker_DefaultAgentLimiterUncapped pins the nil default: without
// a configured limiter, generations start without any admission.
func TestWorker_DefaultAgentLimiterUncapped(t *testing.T) {
	t.Parallel()
	f := newWorkerTestFixture(t)
	starter := newBlockingTaskStarter(false)
	opts := testOptions(t, f, starter)

	chatA := f.createRunningChat(t)
	chatB := f.createRunningChat(t)
	startWorker(t, opts)

	calls := waitTaskCalls(t, starter, 2)
	got := make(map[uuid.UUID]bool, 2)
	for _, call := range calls {
		require.Equal(t, taskKindGeneration, call.kind)
		got[call.input.ChatID] = true
	}
	require.True(t, got[chatA.ID])
	require.True(t, got[chatB.ID])
}

// TestFinishGenerationTurn_MarksReleaseBeforePublish pins that the
// turn-complete mark is visible before the chat:update publish. The
// publish is what spawns a promoted queued message's next generation
// task, so a mark after it races that task's EnsureHeld and can let
// one chat keep its slot across back-to-back turns.
func TestFinishGenerationTurn_MarksReleaseBeforePublish(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitLong)
	f := newTaskTestFixture(t)
	chat := f.createRunningChat(t)
	workerID, runnerID := uuid.New(), uuid.New()
	f.acquireChat(t, chat.ID, workerID, runnerID)

	// Queue a message so FinishTurn promotes it and the chat stays
	// running: the case with no runner-side turn-complete mark.
	machine := chatstate.NewChatMachine(f.db, f.pubsub, chat.ID)
	require.NoError(t, machine.Update(ctx, func(tx *chatstate.Tx, _ database.Store) error {
		_, err := tx.SendMessage(chatstate.SendMessageInput{
			Message:      userTextMessage(t, "queued", f.user.ID, f.model.ID, f.apiKey.ID),
			BusyBehavior: chatstate.BusyBehaviorQueue,
		})
		return err
	}))
	current, err := f.db.GetChatByID(ctx, chat.ID)
	require.NoError(t, err)

	lease := newFakeAgentSlotLease()
	eventsAtPublish := make(chan []string, 1)
	unsub, err := f.rawPS.Subscribe(coderdpubsub.ChatStateUpdateChannel(chat.ID), func(_ context.Context, _ []byte) {
		select {
		case eventsAtPublish <- lease.snapshot():
		default:
		}
	})
	require.NoError(t, err)
	defer unsub()

	recorder := newTaskSideEffectRecorder()
	starter := newTestTaskStarter(t, f, recorder)
	require.NoError(t, starter.finishGenerationTurn(WithAgentSlotLease(ctx, lease), machine, chatWorkerTaskStartInput{
		ChatID:            chat.ID,
		WorkerID:          workerID,
		RunnerID:          runnerID,
		HistoryVersion:    current.HistoryVersion,
		GenerationAttempt: current.GenerationAttempt,
		Status:            database.ChatStatusRunning,
	}, generationDecision{}, generationAttemptNotRequired))

	require.Contains(t, testutil.RequireReceive(ctx, t, eventsAtPublish), "turn_complete")
	latest, err := f.db.GetChatByID(ctx, chat.ID)
	require.NoError(t, err)
	require.Equal(t, database.ChatStatusRunning, latest.Status, "queued message should have been promoted")
}

func TestReleaseAgentSlotOnTransition(t *testing.T) {
	t.Parallel()
	ctx := testutil.Context(t, testutil.WaitShort)

	// Without a lease in ctx the helper is a no-op.
	releaseAgentSlotOnTransition(ctx)

	lease := newFakeAgentSlotLease()
	releaseAgentSlotOnTransition(WithAgentSlotLease(ctx, lease))
	require.Equal(t, []string{"turn_complete"}, lease.snapshot())
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

// TestAwaitSubagentCompletion_PausesAgentSlot pins the wait_agent
// integration: the parent's lease is paused before blocking on the
// child and resumed before returning, and a failed resume surfaces as
// the wait error.
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

	lease := newFakeAgentSlotLease()
	taskCtx := WithAgentSlotLease(ctx, lease)

	done := make(chan error, 1)
	go func() {
		_, _, err := server.awaitSubagentCompletion(taskCtx, parent.ID, child.ID, testutil.WaitLong)
		done <- err
	}()

	// The parent yields its slot before blocking on the child.
	lease.waitForEvent(t, "pause")
	require.NotContains(t, lease.snapshot(), "resume")

	finishTurn(t, f, child.ID)
	require.NoError(t, testutil.RequireReceive(ctx, t, done))
	events := lease.snapshot()
	require.Greater(t, slices.Index(events, "resume"), slices.Index(events, "pause"))
}

func TestAwaitSubagentCompletion_ResumeErrorSurfaces(t *testing.T) {
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

	lease := newFakeAgentSlotLease()
	resumeErr := xerrors.New("slot lost")
	lease.mu.Lock()
	lease.resumeErr = resumeErr
	lease.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, _, err := server.awaitSubagentCompletion(WithAgentSlotLease(ctx, lease), parent.ID, child.ID, testutil.WaitLong)
		done <- err
	}()

	lease.waitForEvent(t, "pause")
	finishTurn(t, f, child.ID)
	require.ErrorIs(t, testutil.RequireReceive(ctx, t, done), resumeErr)
}
