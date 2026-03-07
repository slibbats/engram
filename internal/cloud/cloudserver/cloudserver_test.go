package cloudserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/cloud"
	"github.com/Gentleman-Programming/engram/internal/cloud/auth"
	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
	_ "github.com/lib/pq"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// ─── Test Helpers ───────────────────────────────────────────────────────────

const testJWTSecret = "this-is-a-test-secret-that-is-at-least-32-bytes-long"

// testDSN creates a real Postgres 16-alpine container via dockertest and
// returns a DSN string. Container is cleaned up on test finish.
func testDSN(t *testing.T) string {
	t.Helper()

	if os.Getenv("SKIP_DOCKER_TESTS") == "1" {
		t.Skip("SKIP_DOCKER_TESTS=1, skipping dockertest-based test")
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("could not construct dockertest pool: %v", err)
	}
	if err := pool.Client.Ping(); err != nil {
		t.Fatalf("could not connect to Docker: %v", err)
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
		t.Fatalf("could not start postgres container: %v", err)
	}

	t.Cleanup(func() {
		_ = pool.Purge(resource)
	})

	dsn := fmt.Sprintf("postgres://postgres:test@localhost:%s/engram_test?sslmode=disable",
		resource.GetPort("5432/tcp"))

	if err := pool.Retry(func() error {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Ping()
	}); err != nil {
		t.Fatalf("could not connect to postgres: %v", err)
	}

	return dsn
}

// testSetup creates a CloudStore, auth.Service, and CloudServer backed by
// a real Postgres container. Returns the CloudServer, auth.Service, and a cleanup function.
func testSetup(t *testing.T) (*CloudServer, *auth.Service) {
	t.Helper()

	dsn := testDSN(t)
	cs, err := cloudstore.New(cloud.Config{DSN: dsn, MaxPool: 5})
	if err != nil {
		t.Fatalf("cloudstore.New: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	authSvc, err := auth.NewService(cs, testJWTSecret)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}

	srv := New(cs, authSvc, 0)
	return srv, authSvc
}

// registerUser is a test helper that registers a user via the HTTP handler.
func registerUser(t *testing.T, handler http.Handler, username, email, password string) *auth.AuthResult {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"email":%q,"password":%q}`, username, email, password)
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: expected 201, got %d: %s", username, rec.Code, rec.Body.String())
	}

	var result auth.AuthResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return &result
}

// loginUser is a test helper that logs in via the HTTP handler.
func loginUser(t *testing.T, handler http.Handler, identifier, password string) *auth.AuthResult {
	t.Helper()
	body := fmt.Sprintf(`{"identifier":%q,"password":%q}`, identifier, password)
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: expected 200, got %d: %s", identifier, rec.Code, rec.Body.String())
	}

	var result auth.AuthResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return &result
}

// authReq creates an HTTP request with an Authorization: Bearer header.
func authReq(method, target string, body string, token string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// decodeJSON is a helper to decode JSON response body into a map.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return m
}

// ─── Start/Listen Tests ─────────────────────────────────────────────────────

type stubListener struct{}

func (stubListener) Accept() (net.Conn, error) { return nil, errors.New("not used") }
func (stubListener) Close() error              { return nil }
func (stubListener) Addr() net.Addr            { return &net.TCPAddr{} }

func TestStartReturnsListenError(t *testing.T) {
	srv := New(nil, nil, 9999)
	srv.listen = func(_, _ string) (net.Listener, error) {
		return nil, errors.New("listen failed")
	}

	err := srv.Start()
	if err == nil {
		t.Fatal("expected start to fail on listen error")
	}
}

func TestStartUsesInjectedServe(t *testing.T) {
	srv := New(nil, nil, 9999)
	srv.listen = func(_, _ string) (net.Listener, error) {
		return stubListener{}, nil
	}
	srv.serve = func(ln net.Listener, h http.Handler) error {
		if ln == nil || h == nil {
			t.Fatal("expected listener and handler to be provided")
		}
		return errors.New("serve stopped")
	}

	err := srv.Start()
	if err == nil || err.Error() != "serve stopped" {
		t.Fatalf("expected propagated serve error, got %v", err)
	}
}

// ─── Health Endpoint ────────────────────────────────────────────────────────

func TestHealthNoAuth(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health: expected 200, got %d", rec.Code)
	}

	body := decodeJSON(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("health: expected status=ok, got %v", body["status"])
	}
	if body["service"] != "engram-cloud" {
		t.Fatalf("health: expected service=engram-cloud, got %v", body["service"])
	}
}

func TestHealthDegradedWhenDBUnavailable(t *testing.T) {
	srv, _ := testSetup(t)
	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health degraded: expected 503, got %d: %s", rec.Code, rec.Body.String())
	}

	body := decodeJSON(t, rec)
	if body["status"] != "degraded" {
		t.Fatalf("health degraded: expected status=degraded, got %v", body["status"])
	}
}

// ─── Register + Login Flow ──────────────────────────────────────────────────

func TestRegisterAndLoginFlow(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	// Register
	result := registerUser(t, h, "alice", "alice@test.com", "password123")
	if result.AccessToken == "" || result.RefreshToken == "" {
		t.Fatal("register: expected non-empty tokens")
	}
	if result.UserID == "" {
		t.Fatal("register: expected non-empty user_id")
	}
	if result.Username != "alice" {
		t.Fatalf("register: expected username=alice, got %s", result.Username)
	}

	// Login with same creds
	loginResult := loginUser(t, h, "alice", "password123")
	if loginResult.AccessToken == "" {
		t.Fatal("login: expected non-empty access token")
	}

	emailLoginResult := loginUser(t, h, "alice@test.com", "password123")
	if emailLoginResult.AccessToken == "" {
		t.Fatal("email login: expected non-empty access token")
	}

	// Login with wrong password
	badBody := `{"identifier":"alice","password":"wrong"}`
	badReq := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(badBody))
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("login wrong password: expected 401, got %d", badRec.Code)
	}
}

func TestRegisterInvalidJSON(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader("{invalid"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register invalid json: expected 400, got %d", rec.Code)
	}
}

func TestRegisterMissingFields(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"bob"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register missing fields: expected 400, got %d", rec.Code)
	}
}

func TestRegisterWeakPassword(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	body := `{"username":"bob","email":"bob@test.com","password":"short"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register weak password: expected 400, got %d", rec.Code)
	}
}

func TestRegisterDuplicateUsername(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	registerUser(t, h, "dup", "dup@test.com", "password123")

	body := `{"username":"dup","email":"dup2@test.com","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("register duplicate: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	registerUser(t, h, "first", "shared@test.com", "password123")

	body := `{"username":"second","email":"shared@test.com","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("register duplicate email: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "email") {
		t.Fatalf("expected duplicate email error, got %s", rec.Body.String())
	}
}

// ─── Refresh ────────────────────────────────────────────────────────────────

func TestRefreshToken(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "refreshuser", "refresh@test.com", "password123")

	body := fmt.Sprintf(`{"refresh_token":%q}`, result.RefreshToken)
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := decodeJSON(t, rec)
	if respBody["access_token"] == nil || respBody["access_token"] == "" {
		t.Fatal("refresh: expected non-empty access_token")
	}
}

func TestRefreshInvalidToken(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	body := `{"refresh_token":"this.is.not.valid"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh invalid: expected 401, got %d", rec.Code)
	}
}

// ─── Auth Middleware ────────────────────────────────────────────────────────

func TestAuthMiddlewareMissingHeader(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/sync/search?q=test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth header: expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareExpiredJWT(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	// Use a malformed/expired token
	req := authReq(http.MethodGet, "/sync/search?q=test", "", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ0ZXN0IiwiZXhwIjoxfQ.invalid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired jwt: expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddlewareValidJWT(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "jwtuser", "jwt@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/search?q=test", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should not be 401 (should be 200 since q is provided, even if no results)
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid jwt: should pass auth middleware")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("valid jwt search: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddlewareValidAPIKey(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "apikeyuser", "apikey@test.com", "password123")

	// Generate API key
	genReq := authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	genRec := httptest.NewRecorder()
	h.ServeHTTP(genRec, genReq)

	if genRec.Code != http.StatusCreated {
		t.Fatalf("generate api key: expected 201, got %d: %s", genRec.Code, genRec.Body.String())
	}

	genBody := decodeJSON(t, genRec)
	apiKey, ok := genBody["api_key"].(string)
	if !ok || apiKey == "" {
		t.Fatal("generate api key: expected non-empty api_key")
	}

	// Use API key for auth
	req := authReq(http.MethodGet, "/sync/search?q=test", "", apiKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid api key: should pass auth middleware")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("api key search: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── API Key Management ────────────────────────────────────────────────────

func TestAPIKeyGenerateAndRevoke(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "keyuser", "key@test.com", "password123")

	// Generate
	genReq := authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	genRec := httptest.NewRecorder()
	h.ServeHTTP(genRec, genReq)

	if genRec.Code != http.StatusCreated {
		t.Fatalf("generate key: expected 201, got %d", genRec.Code)
	}

	genBody := decodeJSON(t, genRec)
	apiKey := genBody["api_key"].(string)
	if !strings.HasPrefix(apiKey, "eng_") {
		t.Fatalf("api key should start with eng_, got %s", apiKey)
	}

	// Verify key works
	verifyReq := authReq(http.MethodGet, "/sync/search?q=test", "", apiKey)
	verifyRec := httptest.NewRecorder()
	h.ServeHTTP(verifyRec, verifyReq)
	if verifyRec.Code == http.StatusUnauthorized {
		t.Fatal("api key should work before revocation")
	}

	// Revoke
	revokeReq := authReq(http.MethodDelete, "/auth/api-key", "", result.AccessToken)
	revokeRec := httptest.NewRecorder()
	h.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke key: expected 200, got %d", revokeRec.Code)
	}

	// Verify revoked key fails
	revokedReq := authReq(http.MethodGet, "/sync/search?q=test", "", apiKey)
	revokedRec := httptest.NewRecorder()
	h.ServeHTTP(revokedRec, revokedReq)
	if revokedRec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: expected 401, got %d", revokedRec.Code)
	}
}

func TestAPIKeyRotateInvalidatesOldKey(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "rotate", "rotate@test.com", "password123")

	firstReq := authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	firstRec := httptest.NewRecorder()
	h.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first api key: expected 201, got %d: %s", firstRec.Code, firstRec.Body.String())
	}
	firstKey := decodeJSON(t, firstRec)["api_key"].(string)

	secondReq := authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	secondRec := httptest.NewRecorder()
	h.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusCreated {
		t.Fatalf("second api key: expected 201, got %d: %s", secondRec.Code, secondRec.Body.String())
	}
	secondKey := decodeJSON(t, secondRec)["api_key"].(string)

	if firstKey == "" || secondKey == "" || firstKey == secondKey {
		t.Fatalf("expected distinct non-empty api keys, got first=%q second=%q", firstKey, secondKey)
	}

	staleReq := authReq(http.MethodGet, "/sync/context", "", firstKey)
	staleRec := httptest.NewRecorder()
	h.ServeHTTP(staleRec, staleReq)
	if staleRec.Code != http.StatusUnauthorized {
		t.Fatalf("old api key: expected 401, got %d: %s", staleRec.Code, staleRec.Body.String())
	}

	currentReq := authReq(http.MethodGet, "/sync/context", "", secondKey)
	currentRec := httptest.NewRecorder()
	h.ServeHTTP(currentRec, currentReq)
	if currentRec.Code != http.StatusOK {
		t.Fatalf("new api key: expected 200, got %d: %s", currentRec.Code, currentRec.Body.String())
	}
}

func TestAPIKeyHandlersReturn503WhenStoreUnavailable(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "keyfail", "keyfail@test.com", "password123")

	genReq := authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	genRec := httptest.NewRecorder()
	h.ServeHTTP(genRec, genReq)
	if genRec.Code != http.StatusCreated {
		t.Fatalf("generate api key before close: expected 201, got %d: %s", genRec.Code, genRec.Body.String())
	}
	apiKey := decodeJSON(t, genRec)["api_key"].(string)

	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	genReq = authReq(http.MethodPost, "/auth/api-key", "", result.AccessToken)
	genRec = httptest.NewRecorder()
	h.ServeHTTP(genRec, genReq)
	if genRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("generate api key with closed store: expected 503, got %d: %s", genRec.Code, genRec.Body.String())
	}

	revokeReq := authReq(http.MethodDelete, "/auth/api-key", "", result.AccessToken)
	revokeRec := httptest.NewRecorder()
	h.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("revoke api key with closed store: expected 503, got %d: %s", revokeRec.Code, revokeRec.Body.String())
	}

	registerReq := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"other","email":"other@test.com","password":"password123"}`))
	registerRec := httptest.NewRecorder()
	h.ServeHTTP(registerRec, registerReq)
	if registerRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("register with closed store: expected 503, got %d: %s", registerRec.Code, registerRec.Body.String())
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"keyfail","password":"password123"}`))
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with closed store: expected 503, got %d: %s", loginRec.Code, loginRec.Body.String())
	}

	apiKeyReq := authReq(http.MethodGet, "/sync/search?q=test", "", apiKey)
	apiKeyRec := httptest.NewRecorder()
	h.ServeHTTP(apiKeyRec, apiKeyReq)
	if apiKeyRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("api key auth with closed store: expected 503, got %d: %s", apiKeyRec.Code, apiKeyRec.Body.String())
	}
}

// ─── Push Endpoint ──────────────────────────────────────────────────────────

func TestPushValidChunk(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "pushuser", "push@test.com", "password123")

	chunk := pushRequest{
		ChunkID:   "a3f8c1d2",
		CreatedBy: "pushuser",
		Data: pushData{
			Sessions: []pushSession{
				{ID: "sess-1", Project: "engram", Directory: "/work"},
				{ID: "sess-2", Project: "other", Directory: "/other"},
			},
			Observations: []pushObservation{
				{SessionID: "sess-1", Type: "decision", Title: "Use JWT", Content: "We chose JWT for auth", Project: "engram"},
				{SessionID: "sess-1", Type: "note", Title: "Setup", Content: "Project setup complete", Project: "engram"},
				{SessionID: "sess-2", Type: "observation", Title: "Testing", Content: "Tests pass", Project: "other"},
			},
			Prompts: []pushPrompt{
				{SessionID: "sess-1", Content: "How to implement auth?", Project: "engram"},
			},
		},
	}
	body, _ := json.Marshal(chunk)

	req := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("push: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := decodeJSON(t, rec)
	if respBody["status"] != "accepted" {
		t.Fatalf("push: expected status=accepted, got %v", respBody["status"])
	}
	if respBody["sessions_stored"] != float64(2) {
		t.Fatalf("push: expected sessions_stored=2, got %v", respBody["sessions_stored"])
	}
	if respBody["observations_stored"] != float64(3) {
		t.Fatalf("push: expected observations_stored=3, got %v", respBody["observations_stored"])
	}
	if respBody["prompts_stored"] != float64(1) {
		t.Fatalf("push: expected prompts_stored=1, got %v", respBody["prompts_stored"])
	}
}

func TestPushDuplicateIdempotent(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "dupuser", "dup@push.com", "password123")

	chunk := pushRequest{
		ChunkID:   "b4e9d2f3",
		CreatedBy: "dupuser",
		Data: pushData{
			Sessions: []pushSession{
				{ID: "sess-dup", Project: "test", Directory: "/test"},
			},
			Observations: []pushObservation{
				{SessionID: "sess-dup", Type: "note", Title: "Dup test", Content: "Testing idempotency", Project: "test"},
			},
		},
	}
	body, _ := json.Marshal(chunk)

	// Push first time
	req1 := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("push first: expected 200, got %d", rec1.Code)
	}

	// Push second time (idempotent)
	req2 := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("push duplicate: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestPushNoAuth(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	body := `{"chunk_id":"deadbeef","created_by":"nobody","data":{"sessions":[],"observations":[],"prompts":[]}}`
	req := httptest.NewRequest(http.MethodPost, "/sync/push", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("push no auth: expected 401, got %d", rec.Code)
	}
}

func TestPushOversizedBody(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "oversizeuser", "oversize@test.com", "password123")

	// Create a body larger than 50 MB
	bigData := bytes.Repeat([]byte("x"), maxPushBody+1)
	req := authReq(http.MethodPost, "/sync/push", string(bigData), result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("push oversize: expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPushInvalidJSON(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "badjsonuser", "badjson@test.com", "password123")

	req := authReq(http.MethodPost, "/sync/push", "{invalid json", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("push invalid json: expected 400, got %d", rec.Code)
	}
}

func TestPushInvalidChunkID(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "badchunkuser", "badchunk@test.com", "password123")

	body := `{"chunk_id":"not-hex","created_by":"test","data":{"sessions":[],"observations":[],"prompts":[]}}`
	req := authReq(http.MethodPost, "/sync/push", body, result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("push bad chunk_id: expected 400, got %d", rec.Code)
	}
}

// ─── Pull Manifest ──────────────────────────────────────────────────────────

func TestPullManifestCorrectCount(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "manifest_user", "manifest@test.com", "password123")

	// Push 2 chunks
	for _, id := range []string{"aaaa1111", "bbbb2222"} {
		chunk := pushRequest{
			ChunkID:   id,
			CreatedBy: "manifest_user",
			Data: pushData{
				Sessions: []pushSession{{ID: "s-" + id, Project: "p", Directory: "/d"}},
			},
		}
		body, _ := json.Marshal(chunk)
		req := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("push %s: expected 200, got %d", id, rec.Code)
		}
	}

	// Pull manifest
	req := authReq(http.MethodGet, "/sync/pull", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pull manifest: expected 200, got %d", rec.Code)
	}

	body := decodeJSON(t, rec)
	chunks, ok := body["chunks"].([]any)
	if !ok {
		t.Fatalf("pull manifest: expected chunks array, got %T", body["chunks"])
	}
	if len(chunks) != 2 {
		t.Fatalf("pull manifest: expected 2 chunks, got %d", len(chunks))
	}
}

func TestPullManifestEmptyForNewUser(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "emptyuser", "empty@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/pull", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("pull manifest empty: expected 200, got %d", rec.Code)
	}

	body := decodeJSON(t, rec)
	chunks, ok := body["chunks"].([]any)
	if !ok {
		t.Fatalf("pull manifest empty: expected chunks array, got %T", body["chunks"])
	}
	if len(chunks) != 0 {
		t.Fatalf("pull manifest empty: expected 0 chunks, got %d", len(chunks))
	}
}

// ─── Pull Chunk ─────────────────────────────────────────────────────────────

func TestPullChunkReturnsData(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "pulluser", "pull@test.com", "password123")

	chunk := pushRequest{
		ChunkID:   "cccc3333",
		CreatedBy: "pulluser",
		Data: pushData{
			Sessions: []pushSession{{ID: "s-pull", Project: "test", Directory: "/d"}},
			Observations: []pushObservation{
				{SessionID: "s-pull", Type: "note", Title: "Pull test", Content: "testing pull", Project: "test"},
			},
		},
	}
	body, _ := json.Marshal(chunk)
	pushReq := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push for pull test: expected 200, got %d", pushRec.Code)
	}

	// Pull the chunk
	pullReq := authReq(http.MethodGet, "/sync/pull/cccc3333", "", result.AccessToken)
	pullRec := httptest.NewRecorder()
	h.ServeHTTP(pullRec, pullReq)

	if pullRec.Code != http.StatusOK {
		t.Fatalf("pull chunk: expected 200, got %d: %s", pullRec.Code, pullRec.Body.String())
	}

	// The response should be valid JSON (the raw stored chunk body)
	var pulled map[string]any
	if err := json.NewDecoder(pullRec.Body).Decode(&pulled); err != nil {
		t.Fatalf("pull chunk: decode response: %v", err)
	}
	if pulled["chunk_id"] != "cccc3333" {
		t.Fatalf("pull chunk: expected chunk_id=cccc3333, got %v", pulled["chunk_id"])
	}
}

func TestPullChunkNotFoundForWrongUser(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	userA := registerUser(t, h, "userA", "a@test.com", "password123")
	userB := registerUser(t, h, "userB", "b@test.com", "password123")

	// User A pushes a chunk
	chunk := pushRequest{
		ChunkID:   "dddd4444",
		CreatedBy: "userA",
		Data: pushData{
			Sessions: []pushSession{{ID: "s-a", Project: "test", Directory: "/d"}},
		},
	}
	body, _ := json.Marshal(chunk)
	pushReq := authReq(http.MethodPost, "/sync/push", string(body), userA.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push userA chunk: expected 200, got %d", pushRec.Code)
	}

	// User B tries to pull User A's chunk -> 404
	pullReq := authReq(http.MethodGet, "/sync/pull/dddd4444", "", userB.AccessToken)
	pullRec := httptest.NewRecorder()
	h.ServeHTTP(pullRec, pullReq)

	if pullRec.Code != http.StatusNotFound {
		t.Fatalf("pull wrong user: expected 404, got %d", pullRec.Code)
	}
}

// ─── Search Endpoint ────────────────────────────────────────────────────────

func TestSearchReturnsResults(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "searchuser", "search@test.com", "password123")

	// Push some data to search
	chunk := pushRequest{
		ChunkID:   "eeee5555",
		CreatedBy: "searchuser",
		Data: pushData{
			Sessions: []pushSession{{ID: "s-search", Project: "engram", Directory: "/work"}},
			Observations: []pushObservation{
				{SessionID: "s-search", Type: "decision", Title: "Authentication Design", Content: "We chose JWT authentication for the cloud sync feature", Project: "engram"},
				{SessionID: "s-search", Type: "note", Title: "Database Setup", Content: "PostgreSQL configured with tsvector for full text search", Project: "engram"},
			},
		},
	}
	body, _ := json.Marshal(chunk)
	pushReq := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push for search: expected 200, got %d", pushRec.Code)
	}

	// Search for "authentication"
	searchReq := authReq(http.MethodGet, "/sync/search?q=authentication", "", result.AccessToken)
	searchRec := httptest.NewRecorder()
	h.ServeHTTP(searchRec, searchReq)

	if searchRec.Code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %s", searchRec.Code, searchRec.Body.String())
	}

	respBody := decodeJSON(t, searchRec)
	results, ok := respBody["results"].([]any)
	if !ok {
		t.Fatalf("search: expected results array, got %T", respBody["results"])
	}
	if len(results) == 0 {
		t.Fatal("search: expected non-empty results for 'authentication'")
	}
}

func TestSearchEmptyQueryReturns400(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "emptyquser", "emptyq@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/search", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("search empty q: expected 400, got %d", rec.Code)
	}
}

func TestSearchNoResultsReturnsEmptyArray(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "noresultuser", "noresult@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/search?q=xyznonexistent123", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("search no results: expected 200, got %d", rec.Code)
	}

	respBody := decodeJSON(t, rec)
	results, ok := respBody["results"].([]any)
	if !ok {
		t.Fatalf("search no results: expected results array, got %T", respBody["results"])
	}
	if len(results) != 0 {
		t.Fatalf("search no results: expected empty array, got %d results", len(results))
	}
}

// ─── Context Endpoint ───────────────────────────────────────────────────────

func TestContextReturnsFormattedString(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "ctxuser", "ctx@test.com", "password123")

	// Push some data
	chunk := pushRequest{
		ChunkID:   "ffff6666",
		CreatedBy: "ctxuser",
		Data: pushData{
			Sessions: []pushSession{{ID: "s-ctx", Project: "engram", Directory: "/work"}},
			Observations: []pushObservation{
				{SessionID: "s-ctx", Type: "decision", Title: "Context Test", Content: "Testing the context endpoint", Project: "engram"},
			},
			Prompts: []pushPrompt{
				{SessionID: "s-ctx", Content: "How does context work?", Project: "engram"},
			},
		},
	}
	body, _ := json.Marshal(chunk)
	pushReq := authReq(http.MethodPost, "/sync/push", string(body), result.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push for context: expected 200, got %d", pushRec.Code)
	}

	// Get context
	ctxReq := authReq(http.MethodGet, "/sync/context?project=engram", "", result.AccessToken)
	ctxRec := httptest.NewRecorder()
	h.ServeHTTP(ctxRec, ctxReq)

	if ctxRec.Code != http.StatusOK {
		t.Fatalf("context: expected 200, got %d: %s", ctxRec.Code, ctxRec.Body.String())
	}

	respBody := decodeJSON(t, ctxRec)
	ctx, ok := respBody["context"].(string)
	if !ok {
		t.Fatalf("context: expected context string, got %T", respBody["context"])
	}
	if ctx == "" {
		t.Fatal("context: expected non-empty context string")
	}
	if !strings.Contains(ctx, "Memory from Previous Sessions") {
		t.Fatalf("context: expected formatted context, got: %s", ctx)
	}
}

func TestDataEndpointsReturn503WhenStoreUnavailable(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "closeddb", "closeddb@test.com", "password123")
	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	requests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{name: "search", method: http.MethodGet, target: "/sync/search?q=test"},
		{name: "context", method: http.MethodGet, target: "/sync/context"},
		{name: "pull manifest", method: http.MethodGet, target: "/sync/pull"},
		{name: "pull chunk", method: http.MethodGet, target: "/sync/pull/deadbeef"},
		{name: "push", method: http.MethodPost, target: "/sync/push", body: `{"chunk_id":"deadbeef","created_by":"closeddb","data":{"sessions":[{"id":"sess-1","project":"engram","directory":"/tmp"}],"observations":[],"prompts":[]}}`},
	}

	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			req := authReq(tc.method, tc.target, tc.body, result.AccessToken)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s with closed store: expected 503, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// ─── Error Format ───────────────────────────────────────────────────────────

func TestErrorsReturnStandardJSON(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	// 401 error
	req := httptest.NewRequest(http.MethodGet, "/sync/search?q=test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("error content-type: expected application/json, got %s", rec.Header().Get("Content-Type"))
	}

	body := decodeJSON(t, rec)
	if _, ok := body["error"]; !ok {
		t.Fatal("error response: expected 'error' key in JSON")
	}
}

// ─── Login Edge Cases ───────────────────────────────────────────────────────

func TestLoginInvalidJSON(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader("{bad"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("login invalid json: expected 400, got %d", rec.Code)
	}
}

func TestLoginMissingFields(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("login missing password: expected 400, got %d", rec.Code)
	}
}

func TestLoginRateLimitReturnsRetryAfter(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	registerUser(t, h, "ratelimit", "ratelimit@test.com", "password123")

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"ratelimit","password":"wrong"}`))
		req.RemoteAddr = "198.51.100.10:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"ratelimit","password":"wrong"}`))
	req.RemoteAddr = "198.51.100.10:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after limit, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header, got headers=%v", rec.Header())
	}
}

func TestRegisterRateLimitReturnsRetryAfter(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"username":"user%d","email":"user%d@test.com","password":"password123"}`, i, i)
		req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
		req.RemoteAddr = "198.51.100.20:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d: expected 201, got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"username":"blocked","email":"blocked@test.com","password":"password123"}`))
	req.RemoteAddr = "198.51.100.20:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after register limit, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header, got headers=%v", rec.Header())
	}
}

func TestHelperFunctions(t *testing.T) {
	t.Run("duplicateRegistrationField", func(t *testing.T) {
		if got := duplicateRegistrationField(errors.New(`pq: duplicate key value violates unique constraint "cloud_users_email_key"`)); got != "email" {
			t.Fatalf("expected email duplicate, got %q", got)
		}
		if got := duplicateRegistrationField(errors.New(`pq: duplicate key value violates unique constraint "cloud_users_username_key"`)); got != "username" {
			t.Fatalf("expected username duplicate, got %q", got)
		}
		if got := duplicateRegistrationField(errors.New("something else")); got != "" {
			t.Fatalf("expected no duplicate field, got %q", got)
		}
	})

	t.Run("queryInt", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/sync/search?limit=25", nil)
		if got := queryInt(req, "limit", 10); got != 25 {
			t.Fatalf("expected parsed query int, got %d", got)
		}
		req = httptest.NewRequest(http.MethodGet, "/sync/search?limit=nope", nil)
		if got := queryInt(req, "limit", 10); got != 10 {
			t.Fatalf("expected default query int, got %d", got)
		}
	})

	t.Run("isDBConnectionError", func(t *testing.T) {
		if !isDBConnectionError(errors.New("driver: bad connection")) {
			t.Fatal("expected bad connection to map to db connection error")
		}
		if isDBConnectionError(errors.New("validation failed")) {
			t.Fatal("did not expect validation error to map to db connection error")
		}
	})

	t.Run("extractBearerToken", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Authorization", "Bearer token-value")
		if got := extractBearerToken(req); got != "token-value" {
			t.Fatalf("expected bearer token, got %q", got)
		}
		req.Header.Set("Authorization", "Basic nope")
		if got := extractBearerToken(req); got != "" {
			t.Fatalf("expected empty token for non-bearer auth, got %q", got)
		}
	})

	t.Run("clientIP", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.10, 198.51.100.1")
		if got := clientIP(req); got != "203.0.113.10" {
			t.Fatalf("expected forwarded client IP, got %q", got)
		}

		req = httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "203.0.113.20:9000"
		if got := clientIP(req); got != "203.0.113.20" {
			t.Fatalf("expected remote addr IP, got %q", got)
		}

		req = httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "invalid-addr"
		if got := clientIP(req); got != "invalid-addr" {
			t.Fatalf("expected raw remote addr fallback, got %q", got)
		}
	})
}

func TestRefreshMissingToken(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh missing token: expected 400, got %d", rec.Code)
	}
}

// ─── Mutation Push Endpoint ────────────────────────────────────────────────

func TestMutationPushAcceptsBatch(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutpush", "mutpush@test.com", "password123")

	body := `{
		"mutations": [
			{"entity":"session","entity_key":"s1","op":"upsert","payload":{"id":"s1","project":"engram","directory":"/work"}},
			{"entity":"observation","entity_key":"obs-abc","op":"upsert","payload":{"sync_id":"obs-abc","session_id":"s1","type":"decision","title":"JWT auth","content":"Chose JWT","scope":"project"}},
			{"entity":"prompt","entity_key":"prompt-xyz","op":"upsert","payload":{"sync_id":"prompt-xyz","session_id":"s1","content":"How to auth?","project":"engram"}}
		]
	}`
	req := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("mutation push: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := decodeJSON(t, rec)
	if respBody["accepted"] != float64(3) {
		t.Fatalf("mutation push: expected accepted=3, got %v", respBody["accepted"])
	}
	if respBody["last_seq"] == nil || respBody["last_seq"] == float64(0) {
		t.Fatalf("mutation push: expected non-zero last_seq, got %v", respBody["last_seq"])
	}
}

func TestMutationPushEmptyBatch(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutempty", "mutempty@test.com", "password123")

	body := `{"mutations":[]}`
	req := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("empty push: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := decodeJSON(t, rec)
	if respBody["accepted"] != float64(0) {
		t.Fatalf("empty push: expected accepted=0, got %v", respBody["accepted"])
	}
}

func TestMutationPushInvalidJSON(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutbadjson", "mutbadjson@test.com", "password123")

	req := authReq(http.MethodPost, "/sync/mutations/push", "{invalid", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid json: expected 400, got %d", rec.Code)
	}
}

func TestMutationPushReturnsConflictWhenProjectPaused(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutpaused", "mutpaused@test.com", "password123")
	if err := srv.store.SetProjectSyncEnabled("engram", false, result.UserID, "Security hold"); err != nil {
		t.Fatalf("SetProjectSyncEnabled: %v", err)
	}

	body := `{
		"mutations": [
			{"entity":"session","entity_key":"s1","op":"upsert","payload":{"id":"s1","project":"engram","directory":"/work"}}
		]
	}`
	req := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMutationPushNoAuth(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	body := `{"mutations":[{"entity":"session","entity_key":"s1","op":"upsert","payload":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/sync/mutations/push", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: expected 401, got %d", rec.Code)
	}
}

func TestMutationPushIdempotentRetry(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutidem", "mutidem@test.com", "password123")

	body := `{
		"mutations": [
			{"entity":"session","entity_key":"s1","op":"upsert","payload":{"id":"s1","project":"engram","directory":"/work"}}
		]
	}`

	// First push
	req1 := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first push: expected 200, got %d", rec1.Code)
	}
	resp1 := decodeJSON(t, rec1)
	firstSeq := resp1["last_seq"].(float64)

	// Second push with same content — still succeeds (append-only, seq advances)
	req2 := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second push: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	resp2 := decodeJSON(t, rec2)
	secondSeq := resp2["last_seq"].(float64)

	if secondSeq <= firstSeq {
		t.Fatalf("expected monotonic seq: first=%v second=%v", firstSeq, secondSeq)
	}
}

// ─── Mutation Pull Endpoint ────────────────────────────────────────────────

func TestMutationPullReturnsAfterCursor(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutpull", "mutpull@test.com", "password123")

	// Push 3 mutations
	body := `{
		"mutations": [
			{"entity":"session","entity_key":"s1","op":"upsert","payload":{"id":"s1","project":"p","directory":"/d"}},
			{"entity":"observation","entity_key":"obs-1","op":"upsert","payload":{"sync_id":"obs-1","session_id":"s1","type":"note","title":"t","content":"c","scope":"project"}},
			{"entity":"prompt","entity_key":"pr-1","op":"upsert","payload":{"sync_id":"pr-1","session_id":"s1","content":"hi","project":"p"}}
		]
	}`
	pushReq := authReq(http.MethodPost, "/sync/mutations/push", body, result.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push: expected 200, got %d: %s", pushRec.Code, pushRec.Body.String())
	}

	// Pull all since 0
	pullReq := authReq(http.MethodGet, "/sync/mutations/pull?since_seq=0", "", result.AccessToken)
	pullRec := httptest.NewRecorder()
	h.ServeHTTP(pullRec, pullReq)
	if pullRec.Code != http.StatusOK {
		t.Fatalf("pull: expected 200, got %d: %s", pullRec.Code, pullRec.Body.String())
	}

	pullBody := decodeJSON(t, pullRec)
	mutations, ok := pullBody["mutations"].([]any)
	if !ok {
		t.Fatalf("expected mutations array, got %T", pullBody["mutations"])
	}
	if len(mutations) != 3 {
		t.Fatalf("expected 3 mutations, got %d", len(mutations))
	}
	firstPayload, ok := mutations[0].(map[string]any)["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected first payload object, got %T", mutations[0].(map[string]any)["payload"])
	}
	if firstPayload["id"] != "s1" {
		t.Fatalf("expected first payload id s1, got %v", firstPayload["id"])
	}

	// Extract the seq of the first mutation, then pull since that seq
	firstMut := mutations[0].(map[string]any)
	firstSeq := int64(firstMut["seq"].(float64))

	pullReq2 := authReq(http.MethodGet, fmt.Sprintf("/sync/mutations/pull?since_seq=%d", firstSeq), "", result.AccessToken)
	pullRec2 := httptest.NewRecorder()
	h.ServeHTTP(pullRec2, pullReq2)
	if pullRec2.Code != http.StatusOK {
		t.Fatalf("pull since: expected 200, got %d", pullRec2.Code)
	}

	pullBody2 := decodeJSON(t, pullRec2)
	mutations2 := pullBody2["mutations"].([]any)
	if len(mutations2) != 2 {
		t.Fatalf("expected 2 mutations after cursor, got %d", len(mutations2))
	}
}

func TestMutationPullEmptyForNewUser(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutemptypull", "mutemptypull@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/mutations/pull?since_seq=0", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("empty pull: expected 200, got %d", rec.Code)
	}

	body := decodeJSON(t, rec)
	mutations := body["mutations"].([]any)
	if len(mutations) != 0 {
		t.Fatalf("expected empty mutations, got %d", len(mutations))
	}
	if body["has_more"] != false {
		t.Fatalf("expected has_more=false, got %v", body["has_more"])
	}
}

func TestMutationPullInvalidSinceSeq(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	result := registerUser(t, h, "mutbadseq", "mutbadseq@test.com", "password123")

	req := authReq(http.MethodGet, "/sync/mutations/pull?since_seq=notanumber", "", result.AccessToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since_seq: expected 400, got %d", rec.Code)
	}
}

func TestMutationPullNoAuth(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/sync/mutations/pull?since_seq=0", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth pull: expected 401, got %d", rec.Code)
	}
}

func TestMutationPullUserIsolation(t *testing.T) {
	srv, _ := testSetup(t)
	h := srv.Handler()

	userA := registerUser(t, h, "mutUserA", "muta@test.com", "password123")
	userB := registerUser(t, h, "mutUserB", "mutb@test.com", "password123")

	// User A pushes mutations
	body := `{"mutations":[{"entity":"session","entity_key":"s-a","op":"upsert","payload":{"id":"s-a","project":"projA","directory":"/a"}}]}`
	pushReq := authReq(http.MethodPost, "/sync/mutations/push", body, userA.AccessToken)
	pushRec := httptest.NewRecorder()
	h.ServeHTTP(pushRec, pushReq)
	if pushRec.Code != http.StatusOK {
		t.Fatalf("push A: expected 200, got %d", pushRec.Code)
	}

	// User B pulls — should get 0 mutations
	pullReq := authReq(http.MethodGet, "/sync/mutations/pull?since_seq=0", "", userB.AccessToken)
	pullRec := httptest.NewRecorder()
	h.ServeHTTP(pullRec, pullReq)
	if pullRec.Code != http.StatusOK {
		t.Fatalf("pull B: expected 200, got %d", pullRec.Code)
	}

	pullBody := decodeJSON(t, pullRec)
	mutations := pullBody["mutations"].([]any)
	if len(mutations) != 0 {
		t.Fatalf("expected 0 mutations for user B, got %d", len(mutations))
	}
}
