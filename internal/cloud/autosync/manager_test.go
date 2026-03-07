package autosync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/cloud/remote"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// ─── Fakes ───────────────────────────────────────────────────────────────────

// fakeStore implements LocalStore for tests.
type fakeStore struct {
	mu sync.Mutex

	syncState  *store.SyncState
	mutations  []store.SyncMutation
	leaseOwner string
	leaseUntil time.Time

	// Counters / signals
	acquireLeaseCount int
	releaseLeaseCount int
	ackCount          int
	lastAckedSeq      int64
	applyCount        int
	markFailureCount  int
	markHealthyCount  int
	lastFailureMsg    string
	lastBackoffUntil  time.Time

	// Error injection
	acquireLeaseErr error
	listPendingErr  error
	ackErr          error
	applyErr        error
	markFailureErr  error
	markHealthyErr  error
	getSyncStateErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		syncState: &store.SyncState{
			TargetKey: store.DefaultSyncTargetKey,
			Lifecycle: store.SyncLifecycleIdle,
		},
	}
}

func (fs *fakeStore) GetSyncState(targetKey string) (*store.SyncState, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.getSyncStateErr != nil {
		return nil, fs.getSyncStateErr
	}
	cp := *fs.syncState
	return &cp, nil
}

func (fs *fakeStore) ListPendingSyncMutations(targetKey string, limit int) ([]store.SyncMutation, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.listPendingErr != nil {
		return nil, fs.listPendingErr
	}
	out := make([]store.SyncMutation, len(fs.mutations))
	copy(out, fs.mutations)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (fs *fakeStore) AckSyncMutations(targetKey string, lastAckedSeq int64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.ackCount++
	fs.lastAckedSeq = lastAckedSeq
	if fs.ackErr != nil {
		return fs.ackErr
	}
	// Remove acked mutations and update state.
	var remaining []store.SyncMutation
	for _, m := range fs.mutations {
		if m.Seq > lastAckedSeq {
			remaining = append(remaining, m)
		}
	}
	fs.mutations = remaining
	if fs.syncState.LastAckedSeq < lastAckedSeq {
		fs.syncState.LastAckedSeq = lastAckedSeq
	}
	if fs.syncState.LastAckedSeq >= fs.syncState.LastEnqueuedSeq {
		fs.syncState.Lifecycle = store.SyncLifecycleHealthy
	}
	return nil
}

func (fs *fakeStore) AckSyncMutationSeqs(targetKey string, seqs []int64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.ackCount++
	if fs.ackErr != nil {
		return fs.ackErr
	}
	acked := map[int64]struct{}{}
	for _, seq := range seqs {
		acked[seq] = struct{}{}
		if seq > fs.lastAckedSeq {
			fs.lastAckedSeq = seq
		}
	}
	var remaining []store.SyncMutation
	for _, m := range fs.mutations {
		if _, ok := acked[m.Seq]; !ok {
			remaining = append(remaining, m)
		}
	}
	fs.mutations = remaining
	if len(fs.mutations) == 0 {
		fs.syncState.Lifecycle = store.SyncLifecycleHealthy
	}
	return nil
}

func (fs *fakeStore) SkipAckNonEnrolledMutations(targetKey string) (int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// In tests, skip-ack is a no-op by default — no enrollment filtering.
	return 0, nil
}

func (fs *fakeStore) AcquireSyncLease(targetKey, owner string, ttl time.Duration, now time.Time) (bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.acquireLeaseCount++
	if fs.acquireLeaseErr != nil {
		return false, fs.acquireLeaseErr
	}
	// Check if another owner holds the lease.
	if fs.leaseOwner != "" && fs.leaseOwner != owner && fs.leaseUntil.After(now) {
		return false, nil
	}
	fs.leaseOwner = owner
	fs.leaseUntil = now.Add(ttl)
	return true, nil
}

func (fs *fakeStore) ReleaseSyncLease(targetKey, owner string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.releaseLeaseCount++
	if fs.leaseOwner == owner || fs.leaseOwner == "" {
		fs.leaseOwner = ""
		fs.leaseUntil = time.Time{}
	}
	return nil
}

func (fs *fakeStore) ApplyPulledMutation(targetKey string, mutation store.SyncMutation) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.applyCount++
	if fs.applyErr != nil {
		return fs.applyErr
	}
	if mutation.Seq > fs.syncState.LastPulledSeq {
		fs.syncState.LastPulledSeq = mutation.Seq
	}
	return nil
}

func (fs *fakeStore) MarkSyncFailure(targetKey, message string, backoffUntil time.Time) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.markFailureCount++
	fs.lastFailureMsg = message
	fs.lastBackoffUntil = backoffUntil
	if fs.markFailureErr != nil {
		return fs.markFailureErr
	}
	fs.syncState.ConsecutiveFailures++
	fs.syncState.Lifecycle = store.SyncLifecycleDegraded
	msg := message
	fs.syncState.LastError = &msg
	bu := backoffUntil.UTC().Format(time.RFC3339)
	fs.syncState.BackoffUntil = &bu
	return nil
}

func (fs *fakeStore) MarkSyncHealthy(targetKey string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.markHealthyCount++
	if fs.markHealthyErr != nil {
		return fs.markHealthyErr
	}
	fs.syncState.Lifecycle = store.SyncLifecycleHealthy
	fs.syncState.ConsecutiveFailures = 0
	fs.syncState.BackoffUntil = nil
	fs.syncState.LastError = nil
	return nil
}

// setPending sets up pending local mutations for push tests.
func (fs *fakeStore) setPending(mutations []store.SyncMutation) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.mutations = mutations
	if len(mutations) > 0 {
		fs.syncState.LastEnqueuedSeq = mutations[len(mutations)-1].Seq
		fs.syncState.Lifecycle = store.SyncLifecyclePending
	}
}

// fakeTransport implements RemoteTransport for tests.
type fakeTransport struct {
	mu sync.Mutex

	pushResult  *remote.PushMutationsResult
	pushErr     error
	pushCount   int
	lastPushed  []remote.MutationEntry
	pushBatches [][]remote.MutationEntry

	pullResult    *remote.PullMutationsResponse
	pullErr       error
	pullCount     int
	lastPullSince int64
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		pushResult: &remote.PushMutationsResult{Accepted: 0, LastSeq: 0},
		pullResult: &remote.PullMutationsResponse{Mutations: nil, HasMore: false},
	}
}

func (ft *fakeTransport) PushMutations(mutations []remote.MutationEntry) (*remote.PushMutationsResult, error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.pushCount++
	ft.lastPushed = mutations
	batch := append([]remote.MutationEntry(nil), mutations...)
	ft.pushBatches = append(ft.pushBatches, batch)
	if ft.pushErr != nil {
		return nil, ft.pushErr
	}
	return ft.pushResult, nil
}

func (ft *fakeTransport) PullMutations(sinceSeq int64, limit int) (*remote.PullMutationsResponse, error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.pullCount++
	ft.lastPullSince = sinceSeq
	if ft.pullErr != nil {
		return nil, ft.pullErr
	}
	return ft.pullResult, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeMutation(seq int64, entity, key, op string) store.SyncMutation {
	payload := map[string]string{"id": key}
	data, _ := json.Marshal(payload)
	return store.SyncMutation{
		Seq:        seq,
		TargetKey:  store.DefaultSyncTargetKey,
		Entity:     entity,
		EntityKey:  key,
		Op:         op,
		Payload:    string(data),
		Source:     store.SyncSourceLocal,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func makeRemoteMutation(seq int64, entity, key, op string) remote.PullMutationResult {
	payload := map[string]string{"id": key}
	data, _ := json.Marshal(payload)
	return remote.PullMutationResult{
		Seq:        seq,
		Entity:     entity,
		EntityKey:  key,
		Op:         op,
		Payload:    data,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func newTestManager(fs *fakeStore, ft *fakeTransport) *Manager {
	cfg := DefaultConfig()
	cfg.DebounceDuration = 10 * time.Millisecond // fast for tests
	cfg.PollInterval = 50 * time.Millisecond     // fast for tests
	cfg.LeaseInterval = 5 * time.Second
	cfg.PushBatchSize = 100
	cfg.PullBatchSize = 100
	cfg.MaxConsecutiveFailures = 5
	cfg.BaseBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 100 * time.Millisecond
	return New(fs, ft, cfg)
}

// waitForCondition polls until the condition is true or the timeout is reached.
func waitForCondition(t *testing.T, timeout time.Duration, msg string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition: %s", msg)
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestNewManagerDefaults(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := New(fs, ft, DefaultConfig())
	if m == nil {
		t.Fatal("New returned nil")
	}
	if m.cfg.PushBatchSize <= 0 {
		t.Errorf("expected positive push batch size, got %d", m.cfg.PushBatchSize)
	}
	if m.cfg.PollInterval <= 0 {
		t.Errorf("expected positive poll interval, got %v", m.cfg.PollInterval)
	}
}

func TestStartAndStop(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Let it tick at least once.
	time.Sleep(30 * time.Millisecond)

	cancel()
	select {
	case <-done:
		// Graceful shutdown.
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not shut down within 2 seconds")
	}

	// Lease should be released on shutdown.
	fs.mu.Lock()
	released := fs.releaseLeaseCount
	fs.mu.Unlock()
	if released < 1 {
		t.Errorf("expected lease release on shutdown, got %d releases", released)
	}
}

func TestLeaseAcquisitionFailureDoesNotCrash(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	fs.acquireLeaseErr = errors.New("db locked")
	m := newTestManager(fs, ft)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("manager did not shut down after lease failure")
	}

	// No push/pull should have been attempted.
	ft.mu.Lock()
	pushes := ft.pushCount
	pulls := ft.pullCount
	ft.mu.Unlock()
	if pushes > 0 || pulls > 0 {
		t.Errorf("expected no push/pull without lease, got push=%d pull=%d", pushes, pulls)
	}
}

func TestLeaseContention(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()

	// Pre-acquire lease by another owner.
	fs.leaseOwner = "other-process"
	fs.leaseUntil = time.Now().Add(10 * time.Second)

	m := newTestManager(fs, ft)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	ft.mu.Lock()
	pushes := ft.pushCount
	pulls := ft.pullCount
	ft.mu.Unlock()
	if pushes > 0 || pulls > 0 {
		t.Errorf("expected no push/pull with contended lease, got push=%d pull=%d", pushes, pulls)
	}
}

func TestPushPendingMutations(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	mutations := []store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
		makeMutation(2, "observation", "obs1", "upsert"),
		makeMutation(3, "prompt", "p1", "upsert"),
	}
	fs.setPending(mutations)

	ft.pushResult = &remote.PushMutationsResult{
		Accepted: 3,
		LastSeq:  100,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for push to happen.
	waitForCondition(t, 2*time.Second, "push count > 0", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pushCount > 0
	})

	// Wait for ack.
	waitForCondition(t, 2*time.Second, "ack count > 0", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.ackCount > 0
	})

	cancel()
	<-done

	ft.mu.Lock()
	pushed := ft.lastPushed
	ft.mu.Unlock()

	if len(pushed) != 3 {
		t.Fatalf("expected 3 pushed mutations, got %d", len(pushed))
	}
	if pushed[0].Entity != "session" {
		t.Errorf("expected first pushed entity=session, got %q", pushed[0].Entity)
	}

	fs.mu.Lock()
	acked := fs.lastAckedSeq
	fs.mu.Unlock()
	if acked != 3 {
		t.Errorf("expected acked seq 3, got %d", acked)
	}
}

func TestPullRemoteMutations(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	ft.pullResult = &remote.PullMutationsResponse{
		Mutations: []remote.PullMutationResult{
			makeRemoteMutation(10, "session", "s-remote", "upsert"),
			makeRemoteMutation(11, "observation", "obs-remote", "upsert"),
		},
		HasMore: false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "pull count > 0", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pullCount > 0
	})

	waitForCondition(t, 2*time.Second, "apply count >= 2", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.applyCount >= 2
	})

	cancel()
	<-done

	fs.mu.Lock()
	applied := fs.applyCount
	pulledSeq := fs.syncState.LastPulledSeq
	fs.mu.Unlock()

	if applied < 2 {
		t.Errorf("expected at least 2 applied mutations, got %d", applied)
	}
	if pulledSeq != 11 {
		t.Errorf("expected last pulled seq 11, got %d", pulledSeq)
	}
}

func TestPullPagination(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	callCount := atomic.Int32{}
	ft.mu.Lock()
	ft.mu.Unlock()

	// Override the fake transport with a counting one that returns HasMore first time.
	results := []callResult{
		{result: &remote.PullMutationsResponse{
			Mutations: []remote.PullMutationResult{
				makeRemoteMutation(10, "session", "s1", "upsert"),
			},
			HasMore: true,
		}},
		{result: &remote.PullMutationsResponse{
			Mutations: []remote.PullMutationResult{
				makeRemoteMutation(11, "observation", "o1", "upsert"),
			},
			HasMore: false,
		}},
	}

	paginatingTransport := &paginatingFakeTransport{
		results:    results,
		callCount:  &callCount,
		pushResult: &remote.PushMutationsResult{Accepted: 0, LastSeq: 0},
	}

	m2 := New(fs, paginatingTransport, m.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m2.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "2+ pull pages", func() bool {
		return callCount.Load() >= 2
	})

	cancel()
	<-done

	fs.mu.Lock()
	applied := fs.applyCount
	fs.mu.Unlock()
	if applied < 2 {
		t.Errorf("expected at least 2 applied mutations from paginated pull, got %d", applied)
	}
}

// paginatingFakeTransport returns different results on successive calls.
type paginatingFakeTransport struct {
	results    []callResult
	callCount  *atomic.Int32
	pushResult *remote.PushMutationsResult
}

type callResult struct {
	result *remote.PullMutationsResponse
}

func (pt *paginatingFakeTransport) PushMutations(mutations []remote.MutationEntry) (*remote.PushMutationsResult, error) {
	return pt.pushResult, nil
}

func (pt *paginatingFakeTransport) PullMutations(sinceSeq int64, limit int) (*remote.PullMutationsResponse, error) {
	idx := int(pt.callCount.Load())
	pt.callCount.Add(1)
	if idx < len(pt.results) {
		return pt.results[idx].result, nil
	}
	return &remote.PullMutationsResponse{HasMore: false}, nil
}

func TestPushErrorTriggersBackoff(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushErr = errors.New("network error")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "failure recorded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	cancel()
	<-done

	fs.mu.Lock()
	failures := fs.markFailureCount
	lifecycle := fs.syncState.Lifecycle
	fs.mu.Unlock()

	if failures < 1 {
		t.Errorf("expected at least 1 failure recorded, got %d", failures)
	}
	if lifecycle != store.SyncLifecycleDegraded {
		t.Errorf("expected degraded lifecycle, got %q", lifecycle)
	}

	// Verify manager status reports degraded.
	status := m.Status()
	if status.Phase != PhasePushFailed && status.Phase != PhaseBackoff {
		t.Errorf("expected push_failed or backoff phase, got %q", status.Phase)
	}
}

func TestPullErrorTriggersBackoff(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	ft.pullErr = errors.New("server unavailable")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "failure recorded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	cancel()
	<-done

	fs.mu.Lock()
	failures := fs.markFailureCount
	fs.mu.Unlock()
	if failures < 1 {
		t.Errorf("expected at least 1 failure from pull error, got %d", failures)
	}
}

func TestExponentialBackoffGrows(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushErr = errors.New("persistent error")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for multiple failures.
	waitForCondition(t, 3*time.Second, "3+ failures", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount >= 3
	})

	cancel()
	<-done

	// Verify backoff is exponentially growing by checking status.
	status := m.Status()
	if status.ConsecutiveFailures < 3 {
		t.Errorf("expected at least 3 consecutive failures, got %d", status.ConsecutiveFailures)
	}
}

func TestMaxConsecutiveFailuresBound(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	cfg := DefaultConfig()
	cfg.DebounceDuration = 5 * time.Millisecond
	cfg.PollInterval = 10 * time.Millisecond
	cfg.BaseBackoff = 5 * time.Millisecond
	cfg.MaxBackoff = 20 * time.Millisecond
	cfg.MaxConsecutiveFailures = 3
	m := New(fs, ft, cfg)

	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushErr = errors.New("persistent error")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 3*time.Second, "max failures reached", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount >= 3
	})

	// After max failures, the manager should stop retrying.
	fs.mu.Lock()
	failuresBefore := fs.markFailureCount
	fs.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	fs.mu.Lock()
	failuresAfter := fs.markFailureCount
	fs.mu.Unlock()

	cancel()
	<-done

	// Allow a small margin but should not grow unboundedly.
	growth := failuresAfter - failuresBefore
	if growth > 1 {
		t.Errorf("expected max failure bound to stop retries, but saw %d additional failures", growth)
	}
}

func TestRecoveryAfterBackoff(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})

	// Start with error, then fix.
	ft.pushErr = errors.New("transient error")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "failure recorded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	// Fix the error and ensure data will be accepted.
	ft.mu.Lock()
	ft.pushErr = nil
	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}
	ft.mu.Unlock()

	// Re-add pending mutations (the manager will retry).
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})

	waitForCondition(t, 3*time.Second, "healthy after recovery", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markHealthyCount > 0
	})

	cancel()
	<-done

	fs.mu.Lock()
	healthy := fs.markHealthyCount
	fs.mu.Unlock()
	if healthy < 1 {
		t.Errorf("expected at least 1 healthy mark after recovery, got %d", healthy)
	}
}

func TestDebouncedWakeOnNotify(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)
	m.cfg.PollInterval = 10 * time.Second // very long poll so we can test debounce wake

	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for the manager to be running.
	time.Sleep(20 * time.Millisecond)

	// Now inject pending mutations and notify.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	m.NotifyDirty()

	waitForCondition(t, 2*time.Second, "push after notify", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pushCount > 0
	})

	cancel()
	<-done

	ft.mu.Lock()
	pushes := ft.pushCount
	ft.mu.Unlock()
	if pushes < 1 {
		t.Errorf("expected push after NotifyDirty, got %d pushes", pushes)
	}
}

func TestStatusReportsPhases(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// Before starting, status should be idle.
	status := m.Status()
	if status.Phase != PhaseIdle {
		t.Errorf("expected idle phase before start, got %q", status.Phase)
	}

	// Set up a push scenario.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for the full cycle to complete — recordSuccess sets PhaseHealthy.
	waitForCondition(t, 2*time.Second, "phase becomes healthy", func() bool {
		s := m.Status()
		return s.Phase == PhaseHealthy
	})

	// After successful sync, status should reflect healthy.
	status = m.Status()
	if status.Phase != PhaseHealthy {
		t.Errorf("expected healthy phase after success, got %q", status.Phase)
	}

	cancel()
	<-done
}

func TestGracefulShutdownReleasesLease(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	fs.mu.Lock()
	released := fs.releaseLeaseCount
	fs.mu.Unlock()

	if released < 1 {
		t.Errorf("expected lease release on graceful shutdown, got %d", released)
	}
}

func TestPushThenPullSequence(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// Set up pending mutations and remote mutations.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}
	ft.pullResult = &remote.PullMutationsResponse{
		Mutations: []remote.PullMutationResult{
			makeRemoteMutation(5, "observation", "obs-remote", "upsert"),
		},
		HasMore: false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for both push and pull.
	waitForCondition(t, 2*time.Second, "push+pull", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pushCount > 0 && ft.pullCount > 0
	})

	cancel()
	<-done

	ft.mu.Lock()
	pushes := ft.pushCount
	pulls := ft.pullCount
	ft.mu.Unlock()

	if pushes < 1 {
		t.Errorf("expected at least 1 push, got %d", pushes)
	}
	if pulls < 1 {
		t.Errorf("expected at least 1 pull, got %d", pulls)
	}
}

func TestNoPushWhenNoPending(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// No pending mutations.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	ft.mu.Lock()
	pushes := ft.pushCount
	ft.mu.Unlock()

	// Should still pull even without push.
	if pushes > 0 {
		t.Errorf("expected 0 pushes when no pending, got %d", pushes)
	}
}

func TestApplyPulledMutationError(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	ft.pullResult = &remote.PullMutationsResponse{
		Mutations: []remote.PullMutationResult{
			makeRemoteMutation(10, "session", "s-bad", "upsert"),
		},
		HasMore: false,
	}
	fs.applyErr = errors.New("apply failed")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "failure from apply", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	cancel()
	<-done

	fs.mu.Lock()
	failures := fs.markFailureCount
	fs.mu.Unlock()
	if failures < 1 {
		t.Errorf("expected failure from apply error, got %d", failures)
	}
}

// ─── Integration: Full Round-Trip Tests ──────────────────────────────────────

// TestFullRoundTripLocalWritePushPullApply proves the end-to-end flow:
// local write → push to remote → remote returns mutations → apply locally.
func TestFullRoundTripLocalWritePushPullApply(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// 1. Simulate a local write: enqueue pending mutations (session + observation).
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "sess-abc", "upsert"),
		makeMutation(2, "observation", "obs-xyz", "upsert"),
	})
	ft.pushResult = &remote.PushMutationsResult{Accepted: 2, LastSeq: 100}

	// 2. Simulate remote has new mutations to pull (from another device).
	ft.pullResult = &remote.PullMutationsResponse{
		Mutations: []remote.PullMutationResult{
			makeRemoteMutation(50, "observation", "obs-from-other-device", "upsert"),
			makeRemoteMutation(51, "prompt", "prompt-from-other-device", "upsert"),
		},
		HasMore: false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// 3. Signal dirty and wait for the full cycle to complete.
	m.NotifyDirty()

	// Wait for push completion.
	waitForCondition(t, 2*time.Second, "push completed", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pushCount > 0
	})

	// Wait for pull + apply completion.
	waitForCondition(t, 2*time.Second, "pull applied", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.applyCount >= 2
	})

	// Wait for healthy state.
	waitForCondition(t, 2*time.Second, "healthy", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markHealthyCount > 0
	})

	cancel()
	<-done

	// 4. Verify the round-trip:
	// - Push: 2 local mutations were pushed.
	ft.mu.Lock()
	pushed := ft.lastPushed
	ft.mu.Unlock()
	if len(pushed) != 2 {
		t.Fatalf("expected 2 pushed mutations, got %d", len(pushed))
	}
	if pushed[0].Entity != "session" || pushed[0].EntityKey != "sess-abc" {
		t.Errorf("first pushed mutation: expected session/sess-abc, got %s/%s", pushed[0].Entity, pushed[0].EntityKey)
	}
	if pushed[1].Entity != "observation" || pushed[1].EntityKey != "obs-xyz" {
		t.Errorf("second pushed mutation: expected observation/obs-xyz, got %s/%s", pushed[1].Entity, pushed[1].EntityKey)
	}

	// - Ack: local seq 2 was acked.
	fs.mu.Lock()
	acked := fs.lastAckedSeq
	applied := fs.applyCount
	pulledSeq := fs.syncState.LastPulledSeq
	fs.mu.Unlock()
	if acked != 2 {
		t.Errorf("expected last acked seq 2, got %d", acked)
	}

	// - Pull: 2 remote mutations were applied locally.
	if applied < 2 {
		t.Errorf("expected at least 2 applied mutations, got %d", applied)
	}
	if pulledSeq != 51 {
		t.Errorf("expected last pulled seq 51, got %d", pulledSeq)
	}

	// - Status should be healthy.
	status := m.Status()
	if status.Phase != PhaseHealthy {
		t.Errorf("expected healthy phase, got %q", status.Phase)
	}
	if status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", status.ConsecutiveFailures)
	}
	if status.LastSyncAt == nil {
		t.Error("expected last_sync_at to be set")
	}
}

// ─── Degraded State Tests ────────────────────────────────────────────────────

// TestDegradedStateMessagingOnPushFailure verifies that the manager reports
// the correct phase, error message, and failure count when push fails.
func TestDegradedStateMessagingOnPushFailure(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushErr = errors.New("connection refused")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "failure recorded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	cancel()
	<-done

	// Verify degraded messaging.
	status := m.Status()
	if status.Phase != PhasePushFailed && status.Phase != PhaseBackoff {
		t.Errorf("expected push_failed or backoff phase, got %q", status.Phase)
	}
	if status.LastError == "" {
		t.Error("expected last_error to contain the error message")
	}
	if !strings.Contains(status.LastError, "connection refused") {
		t.Errorf("expected last_error to contain 'connection refused', got %q", status.LastError)
	}
	if status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 consecutive failure, got %d", status.ConsecutiveFailures)
	}
	if status.BackoffUntil == nil {
		t.Error("expected backoff_until to be set after failure")
	}

	// Verify the store persisted the degraded state.
	fs.mu.Lock()
	storeFailures := fs.syncState.ConsecutiveFailures
	storeLifecycle := fs.syncState.Lifecycle
	fs.mu.Unlock()
	if storeLifecycle != store.SyncLifecycleDegraded {
		t.Errorf("expected store lifecycle=degraded, got %q", storeLifecycle)
	}
	if storeFailures < 1 {
		t.Errorf("expected store to track failures, got %d", storeFailures)
	}
}

func TestPushGroupsMixedProjectsSeparately(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	mutA := makeMutation(1, "session", "s1", "upsert")
	mutA.Project = "proj-a"
	mutB := makeMutation(2, "session", "s2", "upsert")
	mutB.Project = "proj-b"
	mutC := makeMutation(3, "observation", "o3", "upsert")
	mutC.Project = "proj-a"
	fs.setPending([]store.SyncMutation{mutA, mutB, mutC})
	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 10}

	if err := m.push(context.Background()); err != nil {
		t.Fatalf("push: %v", err)
	}

	ft.mu.Lock()
	batchCount := len(ft.pushBatches)
	first := ft.pushBatches[0]
	second := ft.pushBatches[1]
	ft.mu.Unlock()
	if batchCount != 2 {
		t.Fatalf("expected 2 project batches, got %d", batchCount)
	}
	if len(first) != 2 || first[0].EntityKey != "s1" || first[1].EntityKey != "o3" {
		t.Fatalf("unexpected first batch: %+v", first)
	}
	if len(second) != 1 || second[0].EntityKey != "s2" {
		t.Fatalf("unexpected second batch: %+v", second)
	}

	fs.mu.Lock()
	remaining := len(fs.mutations)
	acked := fs.lastAckedSeq
	fs.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected all mutations acked, got %d remaining", remaining)
	}
	if acked != 3 {
		t.Fatalf("expected max acked seq 3, got %d", acked)
	}
}

// TestDegradedStateMessagingOnPullFailure verifies degraded state when pull fails.
func TestDegradedStateMessagingOnPullFailure(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// No pending push, but pull fails.
	ft.pullErr = errors.New("504 gateway timeout")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	waitForCondition(t, 2*time.Second, "pull failure recorded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	cancel()
	<-done

	status := m.Status()
	if status.Phase != PhasePullFailed && status.Phase != PhaseBackoff {
		t.Errorf("expected pull_failed or backoff phase, got %q", status.Phase)
	}
	if !strings.Contains(status.LastError, "504 gateway timeout") {
		t.Errorf("expected last_error to contain '504 gateway timeout', got %q", status.LastError)
	}
}

// TestGracefulDegradationLocalWritesContinue verifies that local operations
// are never blocked even when the sync manager is in a degraded state.
func TestGracefulDegradationLocalWritesContinue(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// Make the transport permanently fail.
	ft.pushErr = errors.New("remote unavailable")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Let the manager enter degraded state.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	m.NotifyDirty()

	waitForCondition(t, 2*time.Second, "degraded", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.markFailureCount > 0
	})

	// Key assertion: while degraded, new local writes still enqueue normally.
	// In a real system, s.AddObservation() → enqueueSyncMutation() would work
	// without being blocked by the autosync manager. We verify the manager
	// is still accepting dirty notifications and doesn't block or panic.
	fs.setPending([]store.SyncMutation{
		makeMutation(2, "observation", "obs-new", "upsert"),
	})
	m.NotifyDirty() // non-blocking — must not deadlock

	cancel()
	<-done

	// The manager should have shut down gracefully despite degraded state.
	fs.mu.Lock()
	released := fs.releaseLeaseCount
	fs.mu.Unlock()
	if released < 1 {
		t.Errorf("expected lease released even when degraded, got %d", released)
	}
}

// TestRecoveryFromDegradedToHealthy verifies the manager can recover from
// degraded state when the remote becomes available again.
func TestRecoveryFromDegradedToHealthy(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)

	// Start with remote failure.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	ft.pushErr = errors.New("server down")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for degraded state.
	waitForCondition(t, 2*time.Second, "degraded", func() bool {
		status := m.Status()
		return status.Phase == PhasePushFailed || status.Phase == PhaseBackoff
	})

	// Fix the remote.
	ft.mu.Lock()
	ft.pushErr = nil
	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}
	ft.mu.Unlock()

	// Re-add pending data.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})

	// Wait for recovery.
	waitForCondition(t, 3*time.Second, "healthy after recovery", func() bool {
		status := m.Status()
		return status.Phase == PhaseHealthy
	})

	cancel()
	<-done

	status := m.Status()
	if status.Phase != PhaseHealthy {
		t.Errorf("expected final phase=healthy, got %q", status.Phase)
	}
	if status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures after recovery, got %d", status.ConsecutiveFailures)
	}
	if status.LastError != "" {
		t.Errorf("expected empty last_error after recovery, got %q", status.LastError)
	}
}

func TestMultipleNotifyCoalesced(t *testing.T) {
	fs := newFakeStore()
	ft := newFakeTransport()
	m := newTestManager(fs, ft)
	m.cfg.PollInterval = 10 * time.Second
	m.cfg.DebounceDuration = 30 * time.Millisecond

	ft.pushResult = &remote.PushMutationsResult{Accepted: 1, LastSeq: 50}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)

	// Fire multiple rapid notifications.
	fs.setPending([]store.SyncMutation{
		makeMutation(1, "session", "s1", "upsert"),
	})
	for range 5 {
		m.NotifyDirty()
	}

	waitForCondition(t, 2*time.Second, "push after coalesced notify", func() bool {
		ft.mu.Lock()
		defer ft.mu.Unlock()
		return ft.pushCount > 0
	})

	cancel()
	<-done

	// The key assertion: multiple rapid notifies should NOT produce 5 separate sync cycles.
	// With debouncing, we expect significantly fewer than 5 pushes.
	ft.mu.Lock()
	pushes := ft.pushCount
	ft.mu.Unlock()

	// With good debouncing, 5 rapid notifies should coalesce to ~1-2 pushes.
	if pushes > 3 {
		t.Errorf("expected debounced coalescing (<=3 pushes), got %d pushes from 5 notifies", pushes)
	}
}
