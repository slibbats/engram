package cloudstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/cloud"
	_ "github.com/lib/pq"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// testDSN creates a real Postgres 16-alpine container via dockertest and
// returns a DSN string pointing to it. The container is purged when the
// test finishes (via t.Cleanup).
func testDSN(tb testing.TB) string {
	tb.Helper()

	// Allow skipping in CI environments without Docker.
	if os.Getenv("SKIP_DOCKER_TESTS") == "1" {
		tb.Skip("SKIP_DOCKER_TESTS=1, skipping dockertest-based test")
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		tb.Fatalf("could not construct dockertest pool: %v", err)
	}
	if err := pool.Client.Ping(); err != nil {
		tb.Fatalf("could not connect to Docker: %v", err)
	}

	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16-alpine",
		Env: []string{
			"POSTGRES_PASSWORD=test",
			"POSTGRES_DB=engram_test",
			"POSTGRES_USER=postgres",
		},
	}, func(config *docker.HostConfig) {
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		tb.Fatalf("could not start postgres container: %v", err)
	}

	tb.Cleanup(func() {
		_ = pool.Purge(resource)
	})

	dsn := fmt.Sprintf("postgres://postgres:test@localhost:%s/engram_test?sslmode=disable",
		resource.GetPort("5432/tcp"))

	// Wait for Postgres to be ready.
	if err := pool.Retry(func() error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		tb.Fatalf("could not connect to postgres: %v", err)
	}

	return dsn
}

func newTestStore(tb testing.TB) *CloudStore {
	tb.Helper()
	dsn := testDSN(tb)
	cs, err := New(cloud.Config{DSN: dsn, MaxPool: 5})
	if err != nil {
		tb.Fatalf("New() failed: %v", err)
	}
	tb.Cleanup(func() { cs.Close() })
	return cs
}

// ── Schema Idempotency ─────────────────────────────────────────────────────

func TestSchemaIdempotency(t *testing.T) {
	dsn := testDSN(t)
	cfg := cloud.Config{DSN: dsn, MaxPool: 5}

	// First init.
	cs1, err := New(cfg)
	if err != nil {
		t.Fatalf("first New() failed: %v", err)
	}
	cs1.Close()

	// Second init — must NOT fail (CREATE TABLE IF NOT EXISTS).
	cs2, err := New(cfg)
	if err != nil {
		t.Fatalf("second New() failed (schema not idempotent): %v", err)
	}
	cs2.Close()
}

// ── User CRUD ──────────────────────────────────────────────────────────────

func TestCreateUser(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Username != "alice" {
		t.Errorf("username = %q, want alice", u.Username)
	}
	if u.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", u.Email)
	}
	if u.ID == "" {
		t.Error("user ID should not be empty")
	}
}

func TestCreateUserDuplicateUsername(t *testing.T) {
	cs := newTestStore(t)

	_, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}

	_, err = cs.CreateUser("alice", "different@example.com", "secret456")
	if err == nil {
		t.Fatal("expected error for duplicate username, got nil")
	}
}

func TestCreateUserDuplicateEmail(t *testing.T) {
	cs := newTestStore(t)

	_, err := cs.CreateUser("alice", "shared@example.com", "secret123")
	if err != nil {
		t.Fatalf("first CreateUser: %v", err)
	}

	_, err = cs.CreateUser("bob", "shared@example.com", "secret456")
	if err == nil {
		t.Fatal("expected error for duplicate email, got nil")
	}
}

func TestGetUserByUsername(t *testing.T) {
	cs := newTestStore(t)

	created, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	found, err := cs.GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("found.ID = %q, want %q", found.ID, created.ID)
	}
}

func TestGetUserByUsernameNotFound(t *testing.T) {
	cs := newTestStore(t)

	_, err := cs.GetUserByUsername("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent user")
	}
}

func TestGetUserByAPIKeyHash(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Set an API key hash directly for testing.
	_, err = cs.db.Exec(`UPDATE cloud_users SET api_key_hash = $1 WHERE id = $2`, "testhash123", u.ID)
	if err != nil {
		t.Fatalf("set api key hash: %v", err)
	}

	found, err := cs.GetUserByAPIKeyHash("testhash123")
	if err != nil {
		t.Fatalf("GetUserByAPIKeyHash: %v", err)
	}
	if found.ID != u.ID {
		t.Errorf("found.ID = %q, want %q", found.ID, u.ID)
	}
}

// ── Session Lifecycle ──────────────────────────────────────────────────────

func TestSessionLifecycle(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create session.
	if err := cs.CreateSession(u.ID, "sess-1", "myproject", "/tmp"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Verify it shows up in recent.
	sessions, err := cs.RecentSessions(u.ID, "", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "sess-1" {
		t.Errorf("session ID = %q, want sess-1", sessions[0].ID)
	}
	if sessions[0].Project != "myproject" {
		t.Errorf("session project = %q, want myproject", sessions[0].Project)
	}

	// End session.
	if err := cs.EndSession(u.ID, "sess-1", "done working"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// Verify summary.
	sessions, err = cs.RecentSessions(u.ID, "", 10)
	if err != nil {
		t.Fatalf("RecentSessions after end: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Summary == nil || *sessions[0].Summary != "done working" {
		t.Errorf("session summary = %v, want 'done working'", sessions[0].Summary)
	}
	if sessions[0].EndedAt == nil {
		t.Error("session ended_at should not be nil after EndSession")
	}
}

func TestSessionFilterByProject(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cs.CreateSession(u.ID, "s1", "project-a", "/a")
	cs.CreateSession(u.ID, "s2", "project-b", "/b")

	sessA, err := cs.RecentSessions(u.ID, "project-a", 10)
	if err != nil {
		t.Fatalf("RecentSessions project-a: %v", err)
	}
	if len(sessA) != 1 {
		t.Errorf("expected 1 session for project-a, got %d", len(sessA))
	}
}

// ── Observation CRUD ───────────────────────────────────────────────────────

func TestObservationCRUD(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cs.CreateSession(u.ID, "sess-1", "proj", "/tmp")

	// Add observation.
	obsID, err := cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "sess-1",
		Type:      "decision",
		Title:     "Use Postgres",
		Content:   "Decided to use Postgres for cloud storage.",
		Project:   "proj",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if obsID == 0 {
		t.Error("observation ID should not be 0")
	}

	// Get observation.
	obs, err := cs.GetObservation(u.ID, obsID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs.Title != "Use Postgres" {
		t.Errorf("title = %q, want 'Use Postgres'", obs.Title)
	}
	if obs.Type != "decision" {
		t.Errorf("type = %q, want 'decision'", obs.Type)
	}

	// Recent observations.
	recent, err := cs.RecentObservations(u.ID, "proj", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(recent))
	}
	if recent[0].ID != obsID {
		t.Errorf("recent[0].ID = %d, want %d", recent[0].ID, obsID)
	}
}

func TestObservationSoftDelete(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cs.CreateSession(u.ID, "sess-1", "proj", "/tmp")

	obsID, err := cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "sess-1",
		Type:      "note",
		Title:     "Temporary",
		Content:   "This will be deleted.",
		Project:   "proj",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	// Soft delete.
	if err := cs.DeleteObservation(u.ID, obsID, false); err != nil {
		t.Fatalf("DeleteObservation (soft): %v", err)
	}

	// GetObservation should fail (filters deleted_at IS NULL).
	_, err = cs.GetObservation(u.ID, obsID)
	if err == nil {
		t.Fatal("expected error getting soft-deleted observation")
	}

	// RecentObservations should not include it.
	recent, err := cs.RecentObservations(u.ID, "", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("expected 0 observations after soft delete, got %d", len(recent))
	}
}

func TestObservationHardDelete(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cs.CreateSession(u.ID, "sess-1", "proj", "/tmp")

	obsID, err := cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "sess-1",
		Type:      "note",
		Title:     "Gone forever",
		Content:   "This will be hard deleted.",
		Project:   "proj",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if err := cs.DeleteObservation(u.ID, obsID, true); err != nil {
		t.Fatalf("DeleteObservation (hard): %v", err)
	}

	// Verify the row is actually gone from the table.
	var count int
	cs.db.QueryRow(`SELECT COUNT(*) FROM cloud_observations WHERE id = $1`, obsID).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 rows after hard delete, got %d", count)
	}
}

// ── Prompt CRUD ────────────────────────────────────────────────────────────

func TestPromptCRUD(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cs.CreateSession(u.ID, "sess-1", "proj", "/tmp")

	// Add prompt.
	promptID, err := cs.AddPrompt(u.ID, AddCloudPromptParams{
		SessionID: "sess-1",
		Content:   "How do I implement auth?",
		Project:   "proj",
	})
	if err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if promptID == 0 {
		t.Error("prompt ID should not be 0")
	}

	// Recent prompts.
	prompts, err := cs.RecentPrompts(u.ID, "proj", 10)
	if err != nil {
		t.Fatalf("RecentPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompts))
	}
	if prompts[0].Content != "How do I implement auth?" {
		t.Errorf("prompt content = %q, want 'How do I implement auth?'", prompts[0].Content)
	}
}

func TestPromptFilterByProject(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cs.CreateSession(u.ID, "sess-1", "proj-a", "/a")
	cs.CreateSession(u.ID, "sess-2", "proj-b", "/b")

	cs.AddPrompt(u.ID, AddCloudPromptParams{SessionID: "sess-1", Content: "prompt A", Project: "proj-a"})
	cs.AddPrompt(u.ID, AddCloudPromptParams{SessionID: "sess-2", Content: "prompt B", Project: "proj-b"})

	promptsA, err := cs.RecentPrompts(u.ID, "proj-a", 10)
	if err != nil {
		t.Fatalf("RecentPrompts proj-a: %v", err)
	}
	if len(promptsA) != 1 {
		t.Errorf("expected 1 prompt for proj-a, got %d", len(promptsA))
	}
}

// ── Chunk Idempotency ──────────────────────────────────────────────────────

func TestChunkIdempotency(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	data := []byte(`{"test": true}`)

	// First store.
	if err := cs.StoreChunk(u.ID, "abc12345", "alice", data, 2, 5, 3); err != nil {
		t.Fatalf("first StoreChunk: %v", err)
	}

	// Second store with same chunk_id — should NOT error (ON CONFLICT DO NOTHING).
	if err := cs.StoreChunk(u.ID, "abc12345", "alice", data, 2, 5, 3); err != nil {
		t.Fatalf("second StoreChunk (idempotency): %v", err)
	}

	// Verify only one row.
	chunks, err := cs.ListChunks(u.ID)
	if err != nil {
		t.Fatalf("ListChunks: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk after idempotent store, got %d", len(chunks))
	}
}

func TestGetChunk(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	data := []byte(`{"hello": "world"}`)
	cs.StoreChunk(u.ID, "chunk1", "alice", data, 1, 2, 0)

	got, err := cs.GetChunk(u.ID, "chunk1")
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("chunk data = %q, want %q", got, data)
	}
}

func TestSyncedChunks(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := cs.RecordSyncedChunk(u.ID, "c1"); err != nil {
		t.Fatalf("RecordSyncedChunk: %v", err)
	}

	// Idempotent record.
	if err := cs.RecordSyncedChunk(u.ID, "c1"); err != nil {
		t.Fatalf("RecordSyncedChunk (idempotent): %v", err)
	}

	synced, err := cs.GetSyncedChunks(u.ID)
	if err != nil {
		t.Fatalf("GetSyncedChunks: %v", err)
	}
	if !synced["c1"] {
		t.Error("expected c1 to be in synced chunks")
	}
	if synced["c2"] {
		t.Error("c2 should not be in synced chunks")
	}
}

// ── Data Isolation Between Users ───────────────────────────────────────────

func TestDataIsolation(t *testing.T) {
	cs := newTestStore(t)

	alice, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, err := cs.CreateUser("bob", "bob@example.com", "secret456")
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	// Alice creates a session and observation.
	cs.CreateSession(alice.ID, "alice-sess", "alice-proj", "/alice")
	cs.AddObservation(alice.ID, AddCloudObservationParams{
		SessionID: "alice-sess",
		Type:      "note",
		Title:     "Alice's secret",
		Content:   "This belongs to Alice only.",
		Project:   "alice-proj",
	})

	// Bob creates a session and observation.
	cs.CreateSession(bob.ID, "bob-sess", "bob-proj", "/bob")
	cs.AddObservation(bob.ID, AddCloudObservationParams{
		SessionID: "bob-sess",
		Type:      "note",
		Title:     "Bob's observation",
		Content:   "This belongs to Bob only.",
		Project:   "bob-proj",
	})

	// Alice should only see her observations.
	aliceObs, err := cs.RecentObservations(alice.ID, "", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations alice: %v", err)
	}
	if len(aliceObs) != 1 {
		t.Errorf("alice should have 1 observation, got %d", len(aliceObs))
	}
	if aliceObs[0].Title != "Alice's secret" {
		t.Errorf("alice obs title = %q, want 'Alice's secret'", aliceObs[0].Title)
	}

	// Bob should only see his observations.
	bobObs, err := cs.RecentObservations(bob.ID, "", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations bob: %v", err)
	}
	if len(bobObs) != 1 {
		t.Errorf("bob should have 1 observation, got %d", len(bobObs))
	}

	// Alice should only see her sessions.
	aliceSess, err := cs.RecentSessions(alice.ID, "", 10)
	if err != nil {
		t.Fatalf("RecentSessions alice: %v", err)
	}
	if len(aliceSess) != 1 {
		t.Errorf("alice should have 1 session, got %d", len(aliceSess))
	}

	// Bob can't read Alice's observation by ID.
	if len(aliceObs) > 0 {
		_, err = cs.GetObservation(bob.ID, aliceObs[0].ID)
		if err == nil {
			t.Fatal("bob should NOT be able to get alice's observation")
		}
	}

	// Alice adds a prompt; Bob should not see it.
	cs.AddPrompt(alice.ID, AddCloudPromptParams{
		SessionID: "alice-sess",
		Content:   "Alice's prompt",
		Project:   "alice-proj",
	})
	bobPrompts, err := cs.RecentPrompts(bob.ID, "", 10)
	if err != nil {
		t.Fatalf("RecentPrompts bob: %v", err)
	}
	if len(bobPrompts) != 0 {
		t.Errorf("bob should have 0 prompts, got %d", len(bobPrompts))
	}

	// Chunk isolation.
	cs.StoreChunk(alice.ID, "alice-chunk", "alice", []byte("data"), 1, 1, 0)
	bobChunks, err := cs.ListChunks(bob.ID)
	if err != nil {
		t.Fatalf("ListChunks bob: %v", err)
	}
	if len(bobChunks) != 0 {
		t.Errorf("bob should have 0 chunks, got %d", len(bobChunks))
	}
}

// ── Stats ──────────────────────────────────────────────────────────────────

func TestStats(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Empty stats.
	stats, err := cs.Stats(u.ID)
	if err != nil {
		t.Fatalf("Stats (empty): %v", err)
	}
	if stats.TotalSessions != 0 || stats.TotalObservations != 0 || stats.TotalPrompts != 0 {
		t.Errorf("empty stats should be all 0, got sessions=%d obs=%d prompts=%d",
			stats.TotalSessions, stats.TotalObservations, stats.TotalPrompts)
	}

	// Add some data.
	cs.CreateSession(u.ID, "s1", "proj", "/tmp")
	cs.CreateSession(u.ID, "s2", "proj2", "/tmp2")
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s1", Type: "note", Title: "T1", Content: "C1", Project: "proj",
	})
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s1", Type: "note", Title: "T2", Content: "C2", Project: "proj",
	})
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s2", Type: "note", Title: "T3", Content: "C3", Project: "proj2",
	})
	cs.AddPrompt(u.ID, AddCloudPromptParams{SessionID: "s1", Content: "prompt1", Project: "proj"})

	stats, err = cs.Stats(u.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalSessions != 2 {
		t.Errorf("total sessions = %d, want 2", stats.TotalSessions)
	}
	if stats.TotalObservations != 3 {
		t.Errorf("total observations = %d, want 3", stats.TotalObservations)
	}
	if stats.TotalPrompts != 1 {
		t.Errorf("total prompts = %d, want 1", stats.TotalPrompts)
	}
	if len(stats.Projects) != 2 {
		t.Errorf("projects count = %d, want 2", len(stats.Projects))
	}
}

// ── FormatContext ───────────────────────────────────────────────────────────

func TestFormatContext(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Empty context.
	ctx, err := cs.FormatContext(u.ID, "", "")
	if err != nil {
		t.Fatalf("FormatContext (empty): %v", err)
	}
	if ctx != "" {
		t.Errorf("expected empty context, got %q", ctx)
	}

	// Add data.
	cs.CreateSession(u.ID, "s1", "proj", "/tmp")
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s1", Type: "decision", Title: "Use Postgres", Content: "Decided to use Postgres.", Project: "proj",
	})
	cs.AddPrompt(u.ID, AddCloudPromptParams{SessionID: "s1", Content: "How to do auth?", Project: "proj"})

	ctx, err = cs.FormatContext(u.ID, "proj", "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}

	// Verify format matches local store output structure.
	if !strings.Contains(ctx, "## Memory from Previous Sessions") {
		t.Error("context should contain '## Memory from Previous Sessions'")
	}
	if !strings.Contains(ctx, "### Recent Sessions") {
		t.Error("context should contain '### Recent Sessions'")
	}
	if !strings.Contains(ctx, "### Recent Observations") {
		t.Error("context should contain '### Recent Observations'")
	}
	if !strings.Contains(ctx, "### Recent User Prompts") {
		t.Error("context should contain '### Recent User Prompts'")
	}
	if !strings.Contains(ctx, "Use Postgres") {
		t.Error("context should contain observation title")
	}
	if !strings.Contains(ctx, "[decision]") {
		t.Error("context should contain observation type in brackets")
	}
}

// ── Session with Observation Count ─────────────────────────────────────────

func TestRecentSessionsWithObservationCount(t *testing.T) {
	cs := newTestStore(t)

	u, err := cs.CreateUser("alice", "alice@example.com", "secret123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cs.CreateSession(u.ID, "s1", "proj", "/tmp")
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s1", Type: "note", Title: "T1", Content: "C1", Project: "proj",
	})
	cs.AddObservation(u.ID, AddCloudObservationParams{
		SessionID: "s1", Type: "note", Title: "T2", Content: "C2", Project: "proj",
	})

	sessions, err := cs.RecentSessions(u.ID, "", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].ObservationCount != 2 {
		t.Errorf("observation count = %d, want 2", sessions[0].ObservationCount)
	}
}

// ── Scope Normalization ────────────────────────────────────────────────────

func TestScopeNormalization(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "project"},
		{"project", "project"},
		{"PROJECT", "project"},
		{"personal", "personal"},
		{"PERSONAL", "personal"},
		{"global", "global"},
		{"GLOBAL", "global"},
		{"custom", "custom"},
	}
	for _, tt := range tests {
		got := normalizeScope(tt.in)
		if got != tt.want {
			t.Errorf("normalizeScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── Truncate ───────────────────────────────────────────────────────────────

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short = %q, want 'hello'", got)
	}
	if got := truncate("hello world", 5); got != "hello..." {
		t.Errorf("truncate long = %q, want 'hello...'", got)
	}
	// Unicode safety.
	if got := truncate("こんにちは世界", 3); got != "こんに..." {
		t.Errorf("truncate unicode = %q, want 'こんに...'", got)
	}
}

// ── Mutation Ledger ───────────────────────────────────────────────────────

func TestAppendMutationSingleAndPull(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	payload := json.RawMessage(`{"id":"s1","project":"engram","directory":"/work"}`)
	seq, err := cs.AppendMutation(userID, "session", "s1", "upsert", payload)
	if err != nil {
		t.Fatalf("AppendMutation: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("expected positive seq, got %d", seq)
	}

	result, err := cs.PullMutations(userID, 0, 10)
	if err != nil {
		t.Fatalf("PullMutations: %v", err)
	}
	if len(result.Mutations) != 1 {
		t.Fatalf("expected 1 mutation, got %d", len(result.Mutations))
	}
	if result.Mutations[0].Seq != seq {
		t.Fatalf("seq mismatch: got %d want %d", result.Mutations[0].Seq, seq)
	}
	if result.Mutations[0].Entity != "session" {
		t.Fatalf("entity: got %q", result.Mutations[0].Entity)
	}
	if result.Mutations[0].Op != "upsert" {
		t.Fatalf("op: got %q", result.Mutations[0].Op)
	}
	if result.HasMore {
		t.Fatal("expected has_more=false")
	}
}

func TestAppendMutationBatchAndPullCursor(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	entries := []PushMutationEntry{
		{Entity: "session", EntityKey: "s1", Op: "upsert", Payload: json.RawMessage(`{"id":"s1","project":"p","directory":"/d"}`)},
		{Entity: "observation", EntityKey: "obs-a", Op: "upsert", Payload: json.RawMessage(`{"sync_id":"obs-a","session_id":"s1","type":"note","title":"t","content":"c","scope":"project"}`)},
		{Entity: "prompt", EntityKey: "pr-1", Op: "upsert", Payload: json.RawMessage(`{"sync_id":"pr-1","session_id":"s1","content":"hi","project":"p"}`)},
	}

	result, err := cs.AppendMutationBatch(userID, entries)
	if err != nil {
		t.Fatalf("AppendMutationBatch: %v", err)
	}
	if result.Accepted != 3 {
		t.Fatalf("accepted: got %d", result.Accepted)
	}
	if result.LastSeq <= 0 {
		t.Fatalf("expected positive last_seq, got %d", result.LastSeq)
	}

	// Pull all
	all, err := cs.PullMutations(userID, 0, 100)
	if err != nil {
		t.Fatalf("PullMutations: %v", err)
	}
	if len(all.Mutations) != 3 {
		t.Fatalf("expected 3 mutations, got %d", len(all.Mutations))
	}

	// Pull since first seq — should return 2
	sinceFirst, err := cs.PullMutations(userID, all.Mutations[0].Seq, 100)
	if err != nil {
		t.Fatalf("PullMutations since: %v", err)
	}
	if len(sinceFirst.Mutations) != 2 {
		t.Fatalf("expected 2 mutations after cursor, got %d", len(sinceFirst.Mutations))
	}

	// Pull since last — should return 0
	sinceLast, err := cs.PullMutations(userID, all.Mutations[2].Seq, 100)
	if err != nil {
		t.Fatalf("PullMutations since last: %v", err)
	}
	if len(sinceLast.Mutations) != 0 {
		t.Fatalf("expected 0 mutations after last, got %d", len(sinceLast.Mutations))
	}
}

func TestPullMutationsHasMorePagination(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	// Insert 5 mutations
	for i := 0; i < 5; i++ {
		payload := json.RawMessage(fmt.Sprintf(`{"id":"s%d","project":"p","directory":"/d"}`, i))
		_, err := cs.AppendMutation(userID, "session", fmt.Sprintf("s%d", i), "upsert", payload)
		if err != nil {
			t.Fatalf("AppendMutation %d: %v", i, err)
		}
	}

	// Pull with limit 3 — should get 3 with has_more=true
	result, err := cs.PullMutations(userID, 0, 3)
	if err != nil {
		t.Fatalf("PullMutations: %v", err)
	}
	if len(result.Mutations) != 3 {
		t.Fatalf("expected 3 mutations, got %d", len(result.Mutations))
	}
	if !result.HasMore {
		t.Fatal("expected has_more=true")
	}

	// Pull remaining from last cursor
	result2, err := cs.PullMutations(userID, result.Mutations[2].Seq, 3)
	if err != nil {
		t.Fatalf("PullMutations page 2: %v", err)
	}
	if len(result2.Mutations) != 2 {
		t.Fatalf("expected 2 mutations, got %d", len(result2.Mutations))
	}
	if result2.HasMore {
		t.Fatal("expected has_more=false on last page")
	}
}

func TestAppendMutationBatchEmptyIsNoOp(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	result, err := cs.AppendMutationBatch(userID, nil)
	if err != nil {
		t.Fatalf("AppendMutationBatch empty: %v", err)
	}
	if result.Accepted != 0 {
		t.Fatalf("expected 0 accepted, got %d", result.Accepted)
	}

	pulled, err := cs.PullMutations(userID, 0, 10)
	if err != nil {
		t.Fatalf("PullMutations: %v", err)
	}
	if len(pulled.Mutations) != 0 {
		t.Fatalf("expected 0 mutations, got %d", len(pulled.Mutations))
	}
}

func TestProjectSyncControlsPauseBatchAndPull(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	if err := cs.SetProjectSyncEnabled("engram", false, userID, "Security review"); err != nil {
		t.Fatalf("SetProjectSyncEnabled: %v", err)
	}

	controls, err := cs.ListProjectSyncControls()
	if err != nil {
		t.Fatalf("ListProjectSyncControls: %v", err)
	}
	foundPaused := false
	for _, control := range controls {
		if control.Project == "engram" && !control.SyncEnabled {
			foundPaused = true
			if control.PausedReason == nil || *control.PausedReason != "Security review" {
				t.Fatalf("expected paused reason to persist, got %+v", control.PausedReason)
			}
		}
	}
	if !foundPaused {
		t.Fatal("expected paused project control for engram")
	}

	_, err = cs.AppendMutationBatch(userID, []PushMutationEntry{{
		Entity:    "session",
		EntityKey: "s-paused",
		Op:        "upsert",
		Payload:   json.RawMessage(`{"id":"s-paused","project":"engram","directory":"/work"}`),
	}})
	if !errors.Is(err, ErrProjectSyncPaused) {
		t.Fatalf("expected ErrProjectSyncPaused, got %v", err)
	}

	if _, err := cs.AppendMutation(userID, "session", "s-paused", "upsert", json.RawMessage(`{"id":"s-paused","project":"engram","directory":"/work"}`)); err != nil {
		t.Fatalf("AppendMutation: %v", err)
	}
	if _, err := cs.AppendMutation(userID, "session", "s-open", "upsert", json.RawMessage(`{"id":"s-open","project":"open-proj","directory":"/work"}`)); err != nil {
		t.Fatalf("AppendMutation open project: %v", err)
	}

	pulled, err := cs.PullMutations(userID, 0, 10)
	if err != nil {
		t.Fatalf("PullMutations: %v", err)
	}
	if len(pulled.Mutations) != 1 {
		t.Fatalf("expected only unpaused mutation, got %d", len(pulled.Mutations))
	}
	if pulled.Mutations[0].EntityKey != "s-open" {
		t.Fatalf("expected s-open mutation, got %s", pulled.Mutations[0].EntityKey)
	}
}

func TestPullMutationsUserIsolation(t *testing.T) {
	cs, userA := testStoreAndUser(t)

	// Create user B
	userB, err := cs.CreateUser("mutuser-b", "mutb@test.com", "password123")
	if err != nil {
		t.Fatalf("CreateUser B: %v", err)
	}

	// User A pushes
	_, err = cs.AppendMutation(userA, "session", "s-a", "upsert", json.RawMessage(`{"id":"s-a","project":"projA","directory":"/a"}`))
	if err != nil {
		t.Fatalf("AppendMutation A: %v", err)
	}

	// User B pulls — should get 0
	result, err := cs.PullMutations(userB.ID, 0, 100)
	if err != nil {
		t.Fatalf("PullMutations B: %v", err)
	}
	if len(result.Mutations) != 0 {
		t.Fatalf("expected 0 mutations for user B, got %d", len(result.Mutations))
	}
}

func TestApplyMutationPayloadSession(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	payload := json.RawMessage(`{"id":"apply-s1","project":"engram","directory":"/work"}`)
	err := cs.ApplyMutationPayload(userID, "session", "upsert", payload)
	if err != nil {
		t.Fatalf("ApplyMutationPayload session: %v", err)
	}

	// Verify the session was materialized
	sessions, err := cs.RecentSessions(userID, "engram", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.ID == "apply-s1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("session apply-s1 not found after ApplyMutationPayload")
	}
}

func TestApplyMutationPayloadSessionAcceptsStringifiedJSON(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	payload := json.RawMessage(`"{\"id\":\"apply-s2\",\"project\":\"engram\",\"directory\":\"/compat\"}"`)
	err := cs.ApplyMutationPayload(userID, "session", "upsert", payload)
	if err != nil {
		t.Fatalf("ApplyMutationPayload stringified session: %v", err)
	}

	sessions, err := cs.RecentSessions(userID, "engram", 10)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	for _, s := range sessions {
		if s.ID == "apply-s2" {
			return
		}
	}
	t.Fatal("session apply-s2 not found after stringified ApplyMutationPayload")
}

func TestApplyMutationPayloadObservation(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	// First create the session so FK is satisfied
	err := cs.CreateSession(userID, "obs-sess", "engram", "/work")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	payload := json.RawMessage(`{"sync_id":"obs-apply-1","session_id":"obs-sess","type":"decision","title":"Apply test","content":"Testing apply","scope":"project"}`)
	err = cs.ApplyMutationPayload(userID, "observation", "upsert", payload)
	if err != nil {
		t.Fatalf("ApplyMutationPayload observation: %v", err)
	}

	// Verify materialized
	obs, err := cs.RecentObservations(userID, "", "", 10)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	found := false
	for _, o := range obs {
		if o.Title == "Apply test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("observation not found after ApplyMutationPayload")
	}
}

func TestApplyMutationPayloadUnknownEntity(t *testing.T) {
	cs, userID := testStoreAndUser(t)

	err := cs.ApplyMutationPayload(userID, "unknown", "upsert", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown entity")
	}
	if !strings.Contains(err.Error(), "unknown mutation entity") {
		t.Fatalf("expected 'unknown mutation entity' error, got: %v", err)
	}
}

// testStoreAndUser is a helper that returns a CloudStore and a registered user ID.
func testStoreAndUser(t *testing.T) (*CloudStore, string) {
	t.Helper()
	dsn := testDSN(t)
	cs, err := New(cloud.Config{DSN: dsn, MaxPool: 5})
	if err != nil {
		t.Fatalf("cloudstore.New: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	user, err := cs.CreateUser("mutuser", "mut@test.com", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return cs, user.ID
}
