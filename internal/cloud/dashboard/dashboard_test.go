package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
)

func strPtr(s string) *string { return &s }

// TestHealthEndpoint verifies GET /dashboard/health returns 200 with JSON.
func TestHealthEndpoint(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if !strings.Contains(body, `"subsystem":"dashboard"`) {
		t.Fatalf("expected subsystem=dashboard, got: %s", body)
	}
}

// TestStaticAssets verifies that embedded static files are served.
func TestStaticAssets(t *testing.T) {
	mux := setupTestMux(t)

	tests := []struct {
		path        string
		contentType string
		contains    string
	}{
		{"/dashboard/static/htmx.min.js", "application/javascript", "htmx"},
		{"/dashboard/static/pico.min.css", "text/css", ":root"},
		{"/dashboard/static/styles.css", "text/css", "engram-primary"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s, got %d", tt.path, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.contains) {
				t.Fatalf("expected %s to contain %q", tt.path, tt.contains)
			}
		})
	}
}

// TestLoginPageRendersForm verifies that GET /dashboard/login renders the login form.
func TestLoginPageRendersForm(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/login", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/dashboard/login"`) {
		t.Fatalf("expected login form action, got: %s", body)
	}
	if !strings.Contains(body, `name="identifier"`) {
		t.Fatalf("expected identifier field")
	}
	if !strings.Contains(body, `name="password"`) {
		t.Fatalf("expected password field")
	}
}

// TestLoginSubmitMissingFields verifies empty form submission re-renders with error.
func TestLoginSubmitMissingFields(t *testing.T) {
	mux := setupTestMux(t)

	form := url.Values{}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render), got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "required") {
		t.Fatalf("expected error message about required fields, got: %s", body)
	}
}

// TestUnauthenticatedDashboardRedirectsToLogin verifies that accessing the
// dashboard without a cookie redirects to the login page.
func TestUnauthenticatedDashboardRedirectsToLogin(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	location := rec.Header().Get("Location")
	if location != "/dashboard/login" {
		t.Fatalf("expected redirect to /dashboard/login, got %s", location)
	}
}

// TestLogoutClearsCookie verifies that POST /dashboard/logout clears the cookie
// and redirects to login.
func TestLogoutClearsCookie(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/dashboard/logout", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	location := rec.Header().Get("Location")
	if location != "/dashboard/login" {
		t.Fatalf("expected redirect to /dashboard/login, got %s", location)
	}

	// Verify cookie is cleared (MaxAge=-1 or expires in the past).
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			found = true
			if c.MaxAge >= 0 {
				t.Fatalf("expected MaxAge < 0 to clear cookie, got %d", c.MaxAge)
			}
		}
	}
	if !found {
		t.Fatal("expected engram_session cookie to be set (for clearing)")
	}
}

// TestDashboardRedirectTrailingSlash verifies that GET /dashboard redirects to /dashboard/.
func TestDashboardRedirectTrailingSlash(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Either a 301 redirect to /dashboard/ or a 303 redirect to /dashboard/login (if auth intercepts).
	// Since /dashboard requires auth, it should redirect to login first.
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusMovedPermanently {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
}

// TestMiddlewareInvalidCookieRedirects verifies that an invalid JWT in the cookie
// results in a redirect to login and cookie clearing.
func TestMiddlewareInvalidCookieRedirects(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-valid-jwt"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	location := rec.Header().Get("Location")
	if location != "/dashboard/login" {
		t.Fatalf("expected redirect to /dashboard/login, got %s", location)
	}
}

// TestLoginPageShowsError verifies that LoginPage renders an error message.
func TestLoginPageShowsError(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := LoginPage("something went wrong")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "something went wrong") {
		t.Fatalf("expected error message in output, got: %s", body)
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Fatalf("expected alert role for error, got: %s", body)
	}
}

// TestLayoutRendersNavTabs verifies that Layout includes navigation tabs.
func TestLayoutRendersNavTabs(t *testing.T) {
	content := DashboardHome("test-user-id")
	layout := Layout("Test", "testuser", "dashboard", false, content)

	rec := httptest.NewRecorder()
	if err := layout.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	// Check nav tabs are present
	if !strings.Contains(body, "Dashboard") {
		t.Fatal("expected Dashboard tab")
	}
	if !strings.Contains(body, "Browser") {
		t.Fatal("expected Browser tab")
	}
	if !strings.Contains(body, "Projects") {
		t.Fatal("expected Projects tab")
	}
	if !strings.Contains(body, "Contributors") {
		t.Fatal("expected Contributors tab")
	}
	// Admin tab should NOT be present when isAdmin=false
	if strings.Contains(body, ">Admin<") {
		t.Fatal("Admin tab should not be visible when isAdmin=false")
	}
}

// TestLayoutRendersAdminTab verifies that Layout includes Admin tab when isAdmin=true.
func TestLayoutRendersAdminTab(t *testing.T) {
	content := DashboardHome("test-user-id")
	layout := Layout("Test", "testuser", "dashboard", true, content)

	rec := httptest.NewRecorder()
	if err := layout.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Admin") {
		t.Fatal("expected Admin tab when isAdmin=true")
	}
}

// TestLayoutRendersLogout verifies the logout form is in the header.
func TestLayoutRendersLogout(t *testing.T) {
	content := DashboardHome("test-user-id")
	layout := Layout("Test", "testuser", "dashboard", false, content)

	rec := httptest.NewRecorder()
	if err := layout.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `/dashboard/logout`) {
		t.Fatal("expected logout form action")
	}
	if !strings.Contains(body, "testuser") {
		t.Fatal("expected username in header")
	}
}

// TestEmptyStateComponent verifies the empty state renders correctly.
func TestEmptyStateComponent(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := EmptyState("No Data", "Nothing to show yet")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Data") {
		t.Fatal("expected title in empty state")
	}
	if !strings.Contains(body, "Nothing to show yet") {
		t.Fatal("expected message in empty state")
	}
	if !strings.Contains(body, `class="empty-state"`) {
		t.Fatal("expected empty-state class")
	}
}

// TestStatusBadgeComponent verifies the status badge renders with correct class.
func TestStatusBadgeComponent(t *testing.T) {
	tests := []struct {
		variant string
		label   string
	}{
		{"success", "Active"},
		{"warning", "Pending"},
		{"danger", "Error"},
		{"muted", "Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.variant, func(t *testing.T) {
			rec := httptest.NewRecorder()
			comp := StatusBadge(tt.label, tt.variant)
			if err := comp.Render(context.Background(), rec); err != nil {
				t.Fatalf("render error: %v", err)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.label) {
				t.Fatalf("expected label %q", tt.label)
			}
			expectedClass := "badge-" + tt.variant
			if !strings.Contains(body, expectedClass) {
				t.Fatalf("expected class %q in %s", expectedClass, body)
			}
		})
	}
}

// TestLoginPageNoErrorHidesAlert verifies no error div when errorMsg is empty.
func TestLoginPageNoErrorHidesAlert(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := LoginPage("")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `role="alert"`) {
		t.Fatal("expected no alert when error is empty")
	}
}

// ─── Phase 4: Dashboard Stats Partial ───────────────────────────────────────

// TestDashboardStatsPartialEmpty verifies the empty state renders when no stats.
func TestDashboardStatsPartialEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := DashboardStatsPartial(nil, nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Synced Projects Yet") {
		t.Fatal("expected empty state message for no stats")
	}
}

// TestDashboardStatsPartialWithData verifies stats table renders with data.
func TestDashboardStatsPartialWithData(t *testing.T) {
	lastActivity := "2025-01-15T10:00:00Z"
	stats := []cloudstore.ProjectStat{
		{Project: "engram", SessionCount: 5, ObservationCount: 20, PromptCount: 3, LastActivity: &lastActivity},
		{Project: "other-project", SessionCount: 2, ObservationCount: 10, PromptCount: 1, LastActivity: nil},
	}
	projects := []string{"engram", "other-project"}

	rec := httptest.NewRecorder()
	comp := DashboardStatsPartial(stats, projects)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "engram") {
		t.Fatal("expected project name 'engram'")
	}
	if !strings.Contains(body, "other-project") {
		t.Fatal("expected project name 'other-project'")
	}
	if !strings.Contains(body, "Project Activity") {
		t.Fatal("expected 'Project Activity' heading")
	}
}

// TestDashboardHomeRendersHtmxTrigger verifies the htmx stats loader is present.
func TestDashboardHomeRendersHtmxTrigger(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := DashboardHome("gentleman")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `hx-get="/dashboard/stats"`) {
		t.Fatal("expected htmx trigger for /dashboard/stats")
	}
	if !strings.Contains(body, `hx-trigger="load"`) {
		t.Fatal("expected hx-trigger=load")
	}
}

// ─── Phase 5: Knowledge Browser ─────────────────────────────────────────────

// TestBrowserPageRendersControls verifies the browser page has filter controls.
func TestBrowserPageRendersControls(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := BrowserPage([]string{"project-a", "project-b"}, []string{"bugfix", "session_summary"}, "", "", "bugfix")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Knowledge Browser") {
		t.Fatal("expected page title")
	}
	if !strings.Contains(body, "project-a") {
		t.Fatal("expected project-a in dropdown")
	}
	if !strings.Contains(body, "project-b") {
		t.Fatal("expected project-b in dropdown")
	}
	if !strings.Contains(body, `name="q"`) {
		t.Fatal("expected search input")
	}
	if !strings.Contains(body, "session_summary") {
		t.Fatal("expected dynamic type pill")
	}
	if !strings.Contains(body, "Observations") {
		t.Fatal("expected Observations subtab")
	}
	if !strings.Contains(body, "Sessions") {
		t.Fatal("expected Sessions subtab")
	}
	if !strings.Contains(body, "Prompts") {
		t.Fatal("expected Prompts subtab")
	}
}

// TestObservationsPartialEmpty verifies empty state for observations.
func TestObservationsPartialEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := ObservationsPartial(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Observations") {
		t.Fatal("expected empty state for observations")
	}
}

// TestObservationsPartialWithData verifies observations render with data.
func TestObservationsPartialWithData(t *testing.T) {
	project := "test-project"
	obs := []cloudstore.CloudObservation{
		{
			ID: 1, Title: "Fixed auth bug", Type: "bugfix", Content: "Detailed fix content",
			Project: &project, Scope: "project", CreatedAt: "2025-01-15T10:00:00Z",
		},
	}
	rec := httptest.NewRecorder()
	comp := ObservationsPartial(obs)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fixed auth bug") {
		t.Fatal("expected observation title")
	}
	if !strings.Contains(body, "bugfix") {
		t.Fatal("expected observation type")
	}
}

// TestSessionsPartialEmpty verifies empty state for sessions.
func TestSessionsPartialEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := SessionsPartial(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Sessions") {
		t.Fatal("expected empty state for sessions")
	}
}

// TestSessionsPartialWithData verifies sessions render with data.
func TestSessionsPartialWithData(t *testing.T) {
	sessions := []cloudstore.CloudSessionSummary{
		{ID: "sess-1", Project: "engram", StartedAt: "2025-01-15T10:00:00Z", ObservationCount: 5},
	}
	rec := httptest.NewRecorder()
	comp := SessionsPartial(sessions)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "engram") {
		t.Fatal("expected session project name")
	}
	if !strings.Contains(body, "Active") {
		t.Fatal("expected Active badge for session with nil EndedAt")
	}
}

// TestPromptsPartialEmpty verifies empty state for prompts.
func TestPromptsPartialEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := PromptsPartial(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Prompts") {
		t.Fatal("expected empty state for prompts")
	}
}

// TestPromptsPartialWithData verifies prompts render with data.
func TestPromptsPartialWithData(t *testing.T) {
	prompts := []cloudstore.CloudPrompt{
		{ID: 1, Content: "help me fix the auth", Project: "engram", CreatedAt: "2025-01-15T10:00:00Z"},
	}
	rec := httptest.NewRecorder()
	comp := PromptsPartial(prompts)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "help me fix the auth") {
		t.Fatal("expected prompt content")
	}
	if !strings.Contains(body, "engram") {
		t.Fatal("expected prompt project badge")
	}
}

// ─── Phase 6: Projects Tab ─────────────────────────────────────────────────

// TestProjectsPageEmpty verifies the projects page empty state.
func TestProjectsPageEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := ProjectsPage(nil, nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Projects") {
		t.Fatal("expected empty state for projects")
	}
}

// TestProjectsPageWithData verifies the projects page renders project cards.
func TestProjectsPageWithData(t *testing.T) {
	lastActivity := "2025-01-15T10:00:00Z"
	stats := []cloudstore.ProjectStat{
		{Project: "engram", SessionCount: 10, ObservationCount: 50, PromptCount: 5, LastActivity: &lastActivity},
	}
	rec := httptest.NewRecorder()
	comp := ProjectsPage(stats, map[string]cloudstore.ProjectSyncControl{"engram": {Project: "engram", SyncEnabled: false, PausedReason: strPtr("Security review")}})
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "engram") {
		t.Fatal("expected project name")
	}
	if !strings.Contains(body, `href="/dashboard/projects/engram"`) {
		t.Fatal("expected project detail link")
	}
	if !strings.Contains(body, "Sessions") {
		t.Fatal("expected Sessions stat label")
	}
	if !strings.Contains(body, "Paused") {
		t.Fatal("expected paused badge")
	}
}

// TestProjectDetailPage verifies the project detail view renders all sections.
func TestProjectDetailPage(t *testing.T) {
	project := "engram"
	stat := &cloudstore.ProjectStat{Project: "engram", SessionCount: 3, ObservationCount: 15, PromptCount: 2}

	rec := httptest.NewRecorder()
	comp := ProjectDetailPage(project, stat, &cloudstore.ProjectSyncControl{Project: "engram", SyncEnabled: false, PausedReason: strPtr("Security review"), UpdatedAt: "2025-01-15T10:00:00Z", UpdatedBy: strPtr("gentleman")}, nil, nil, nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "engram") {
		t.Fatal("expected project name in header")
	}
	if !strings.Contains(body, "Recent Sessions") {
		t.Fatal("expected Recent Sessions section")
	}
	if !strings.Contains(body, "Recent Observations") {
		t.Fatal("expected Recent Observations section")
	}
	if !strings.Contains(body, "Recent Prompts") {
		t.Fatal("expected Recent Prompts section")
	}
	if !strings.Contains(body, "Security review") {
		t.Fatal("expected pause reason")
	}
}

// ─── Phase 7: Contributors Tab ──────────────────────────────────────────────

// TestContributorsPageEmpty verifies the empty state.
func TestContributorsPageEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := ContributorsPage(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Contributors") {
		t.Fatal("expected empty state")
	}
}

// TestContributorsPageWithData verifies contributors table renders.
func TestContributorsPageWithData(t *testing.T) {
	lastSync := "2025-01-15T10:00:00Z"
	contributors := []cloudstore.ContributorStat{
		{UserID: "u1", Username: "alice", Email: "alice@example.com", SessionCount: 5, ObservationCount: 20, LastSync: &lastSync},
		{UserID: "u2", Username: "bob", Email: "bob@example.com", SessionCount: 2, ObservationCount: 8, LastSync: nil},
	}
	rec := httptest.NewRecorder()
	comp := ContributorsPage(contributors)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice") {
		t.Fatal("expected alice username")
	}
	if !strings.Contains(body, "bob") {
		t.Fatal("expected bob username")
	}
	if !strings.Contains(body, "alice@example.com") {
		t.Fatal("expected alice email")
	}
	if !strings.Contains(body, "Never") {
		t.Fatal("expected 'Never' for bob's nil last sync")
	}
	if !strings.Contains(body, "/dashboard/contributors/u1") {
		t.Fatal("expected contributor detail link")
	}
}

func TestPromptDetailPageRendersLinkedSession(t *testing.T) {
	prompt := &cloudstore.CloudPrompt{ID: 7, SessionID: "sess-7", Content: "How do we sync this?", Project: "engram", CreatedAt: "2025-01-15T10:00:00Z"}
	session := &cloudstore.CloudSession{ID: "sess-7", Project: "engram", StartedAt: "2025-01-15T09:55:00Z"}
	related := []cloudstore.CloudPrompt{{ID: 8, SessionID: "sess-7", Content: "What about retries?", Project: "engram", CreatedAt: "2025-01-15T10:05:00Z"}}
	rec := httptest.NewRecorder()
	comp := PromptDetailPage(prompt, session, related)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dashboard/sessions/sess-7") {
		t.Fatal("expected linked session")
	}
	if !strings.Contains(body, "What about retries?") {
		t.Fatal("expected related prompt")
	}
}

func TestContributorDetailPageRendersConnectedData(t *testing.T) {
	user := &cloudstore.CloudUser{ID: "u1", Username: "alice", Email: "alice@example.com", CreatedAt: "2025-01-15T10:00:00Z"}
	contributor := &cloudstore.ContributorStat{UserID: "u1", Username: "alice", SessionCount: 5, ObservationCount: 20, LastSync: strPtr("2025-01-15T11:00:00Z")}
	sessions := []cloudstore.CloudSessionSummary{{ID: "s1", Project: "engram", StartedAt: "2025-01-15T10:00:00Z", ObservationCount: 2}}
	observations := []cloudstore.CloudObservation{{ID: 1, SessionID: "s1", Title: "Fix sync", Type: "bugfix", CreatedAt: "2025-01-15T10:05:00Z"}}
	prompts := []cloudstore.CloudPrompt{{ID: 2, SessionID: "s1", Content: "How do we fix sync?", CreatedAt: "2025-01-15T10:06:00Z"}}
	rec := httptest.NewRecorder()
	comp := ContributorDetailPage(user, contributor, sessions, observations, prompts)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alice@example.com") {
		t.Fatal("expected contributor email")
	}
	if !strings.Contains(body, "Fix sync") {
		t.Fatal("expected observation list")
	}
	if !strings.Contains(body, "How do we fix sync?") {
		t.Fatal("expected prompt list")
	}
}

func TestRenderStructuredContentFormatsSections(t *testing.T) {
	raw := "**What**: Added docs\n\n**Why**: User asked for it"
	out := renderStructuredContent(raw)
	if !strings.Contains(out, `<section class="structured-block">`) {
		t.Fatal("expected structured block wrapper")
	}
	if !strings.Contains(out, "<h4>What</h4>") {
		t.Fatal("expected What heading")
	}
	if !strings.Contains(out, "Added docs") {
		t.Fatal("expected What content")
	}
}

func TestRenderInlineStructuredPreviewFlattensStructuredMemory(t *testing.T) {
	raw := "**What**: Added docs\n\n**Why**: User asked for it"
	preview := renderInlineStructuredPreview(raw, 120)
	if !strings.Contains(preview, "What: Added docs") {
		t.Fatal("expected flattened What preview")
	}
	if !strings.Contains(preview, "Why: User asked for it") {
		t.Fatal("expected flattened Why preview")
	}
}

func TestRenderStructuredContentFormatsHeadingSections(t *testing.T) {
	raw := "## Goal\nFix sync\n\n## Instructions\nKeep local-first\n\n## Discoveries\n- Backfill was missing"
	out := renderStructuredContent(raw)
	if !strings.Contains(out, "<h4>Goal</h4>") {
		t.Fatal("expected Goal heading")
	}
	if !strings.Contains(out, "Keep local-first") {
		t.Fatal("expected Instructions content")
	}
}

func TestRenderInlineStructuredPreviewFlattensHeadingSections(t *testing.T) {
	raw := "## Goal\nFix sync\n\n## Instructions\nKeep local-first"
	preview := renderInlineStructuredPreview(raw, 160)
	if !strings.Contains(preview, "Goal: Fix sync") {
		t.Fatal("expected flattened Goal preview")
	}
	if !strings.Contains(preview, "Instructions: Keep local-first") {
		t.Fatal("expected flattened Instructions preview")
	}
}

// ─── Phase 8: Admin Views ───────────────────────────────────────────────────

// TestAdminPageRendersHealth verifies the admin overview renders health data.
func TestAdminPageRendersHealth(t *testing.T) {
	health := &cloudstore.SystemHealthInfo{
		DBConnected:    true,
		TotalUsers:     5,
		TotalSessions:  100,
		TotalMemories:  500,
		TotalPrompts:   50,
		TotalMutations: 200,
	}
	rec := httptest.NewRecorder()
	comp := AdminPage(health, []cloudstore.ProjectSyncControl{{Project: "engram", SyncEnabled: false}})
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Admin") {
		t.Fatal("expected Admin heading")
	}
	if !strings.Contains(body, "Connected") {
		t.Fatal("expected Connected badge")
	}
	if !strings.Contains(body, "Users") {
		t.Fatal("expected Users admin nav link")
	}
}

// TestAdminUsersPageEmpty verifies empty user list.
func TestAdminUsersPageEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := AdminUsersPage(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No Users") {
		t.Fatal("expected empty state for users")
	}
}

// TestAdminUsersPageWithData verifies users table renders.
func TestAdminUsersPageWithData(t *testing.T) {
	apiKeyHash := "some-hash"
	users := []cloudstore.CloudUser{
		{ID: "u1", Username: "admin", Email: "admin@example.com", CreatedAt: "2025-01-01", APIKeyHash: &apiKeyHash},
		{ID: "u2", Username: "user1", Email: "user1@example.com", CreatedAt: "2025-01-10", APIKeyHash: nil},
	}
	rec := httptest.NewRecorder()
	comp := AdminUsersPage(users)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "admin") {
		t.Fatal("expected admin username")
	}
	if !strings.Contains(body, "user1") {
		t.Fatal("expected user1 username")
	}
	// User with API key should show "Active"
	if !strings.Contains(body, "Active") {
		t.Fatal("expected Active badge for user with API key")
	}
	// User without API key should show "None"
	if !strings.Contains(body, "None") {
		t.Fatal("expected None badge for user without API key")
	}
}

// TestAdminHealthPage verifies the health detail page renders.
func TestAdminHealthPage(t *testing.T) {
	health := &cloudstore.SystemHealthInfo{
		DBConnected:    true,
		TotalUsers:     3,
		TotalSessions:  50,
		TotalMemories:  200,
		TotalPrompts:   25,
		TotalMutations: 100,
		DBVersion:      "PostgreSQL 16.1",
	}
	rec := httptest.NewRecorder()
	comp := AdminHealthPage(health)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "System Health") {
		t.Fatal("expected System Health heading")
	}
	if !strings.Contains(body, "PostgreSQL 16.1") {
		t.Fatal("expected DB version")
	}
	if !strings.Contains(body, "Connected") {
		t.Fatal("expected Connected status")
	}
}

// TestAdminHealthPageNil verifies the health page handles nil gracefully.
func TestAdminHealthPageNil(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := AdminHealthPage(nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Unavailable") {
		t.Fatal("expected Unavailable message when health is nil")
	}
}

// TestAdminForbiddenPage verifies the 403 page renders.
func TestAdminForbiddenPage(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := AdminForbidden()
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access Denied") {
		t.Fatal("expected Access Denied message")
	}
}

// ─── Auth route tests (unauthenticated redirects for new routes) ────────────

// TestUnauthenticatedBrowserRedirects verifies browser redirects to login.
func TestUnauthenticatedBrowserRedirects(t *testing.T) {
	mux := setupTestMux(t)

	routes := []string{
		"/dashboard/browser",
		"/dashboard/browser/observations",
		"/dashboard/browser/sessions",
		"/dashboard/browser/prompts",
		"/dashboard/sessions/test-session",
		"/dashboard/observations/42",
		"/dashboard/prompts/42",
		"/dashboard/projects",
		"/dashboard/contributors",
		"/dashboard/contributors/test-user",
		"/dashboard/admin",
		"/dashboard/admin/users",
		"/dashboard/admin/health",
		"/dashboard/stats",
	}

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("expected 303 redirect for %s, got %d", route, rec.Code)
			}
			location := rec.Header().Get("Location")
			if location != "/dashboard/login" {
				t.Fatalf("expected redirect to /dashboard/login for %s, got %s", route, location)
			}
		})
	}
}

// ─── Helper function tests ──────────────────────────────────────────────────

// TestTruncateContent verifies truncation behavior.
func TestTruncateContent(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"short", 10, "short"},
		{"a longer string", 5, "a lon..."},
		{"", 5, ""},
		{"exact", 5, "exact"},
	}
	for _, tt := range tests {
		result := truncateContent(tt.input, tt.max)
		if result != tt.expected {
			t.Errorf("truncateContent(%q, %d) = %q, want %q", tt.input, tt.max, result, tt.expected)
		}
	}
}

// TestTypeBadgeVariant verifies badge variant mapping.
func TestTypeBadgeVariant(t *testing.T) {
	tests := []struct {
		obsType  string
		expected string
	}{
		{"decision", "success"},
		{"architecture", "success"},
		{"bugfix", "danger"},
		{"discovery", "warning"},
		{"learning", "warning"},
		{"manual", "muted"},
		{"unknown", "muted"},
	}
	for _, tt := range tests {
		result := typeBadgeVariant(tt.obsType)
		if result != tt.expected {
			t.Errorf("typeBadgeVariant(%q) = %q, want %q", tt.obsType, result, tt.expected)
		}
	}
}

// ─── Phase 9: Additional Coverage — Middleware, Auth, Admin ─────────────────

// TestMiddlewareContextHelpers verifies getUserIDFromContext, getUsernameFromContext,
// getEmailFromContext return empty strings when context has no values set.
func TestMiddlewareContextHelpers(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if got := getUserIDFromContext(req); got != "" {
		t.Fatalf("expected empty userID, got %q", got)
	}
	if got := getUsernameFromContext(req); got != "" {
		t.Fatalf("expected empty username, got %q", got)
	}
	if got := getEmailFromContext(req); got != "" {
		t.Fatalf("expected empty email, got %q", got)
	}
}

// TestMiddlewareContextHelpersWithValues verifies context helpers return
// values when context is populated.
func TestMiddlewareContextHelpersWithValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxUserID, "user-123")
	ctx = context.WithValue(ctx, ctxUsername, "alice")
	ctx = context.WithValue(ctx, ctxEmail, "alice@example.com")
	req = req.WithContext(ctx)

	if got := getUserIDFromContext(req); got != "user-123" {
		t.Fatalf("expected user-123, got %q", got)
	}
	if got := getUsernameFromContext(req); got != "alice" {
		t.Fatalf("expected alice, got %q", got)
	}
	if got := getEmailFromContext(req); got != "alice@example.com" {
		t.Fatalf("expected alice@example.com, got %q", got)
	}
}

// TestMiddlewareNoCookie verifies that a request without any cookie gets redirected.
func TestMiddlewareNoCookie(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
}

// TestMiddlewareEmptyCookieValue verifies that an empty cookie value redirects.
func TestMiddlewareEmptyCookieValue(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ""})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
}

// TestIsAdminEmptyConfig verifies that isAdmin returns false when AdminEmail is empty.
func TestIsAdminEmptyConfig(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: ""}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxEmail, "admin@example.com")
	req = req.WithContext(ctx)

	if h.isAdmin(req) {
		t.Fatal("expected isAdmin=false when AdminEmail is empty")
	}
}

// TestIsAdminMatchesEmail verifies admin check via email match.
func TestIsAdminMatchesEmail(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: "admin@example.com"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxEmail, "admin@example.com")
	req = req.WithContext(ctx)

	if !h.isAdmin(req) {
		t.Fatal("expected isAdmin=true when email matches AdminEmail")
	}
}

// TestIsAdminMismatchEmail verifies admin check fails when email doesn't match.
func TestIsAdminMismatchEmail(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: "admin@example.com"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxEmail, "user@example.com")
	ctx = context.WithValue(ctx, ctxUsername, "user")
	req = req.WithContext(ctx)

	if h.isAdmin(req) {
		t.Fatal("expected isAdmin=false when email doesn't match")
	}
}

// TestIsAdminFallsBackToUsername verifies admin check via username fallback
// (when email is empty but username matches AdminEmail).
func TestIsAdminFallsBackToUsername(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: "admin@example.com"}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), ctxEmail, "")
	ctx = context.WithValue(ctx, ctxUsername, "admin@example.com")
	req = req.WithContext(ctx)

	if !h.isAdmin(req) {
		t.Fatal("expected isAdmin=true when username matches AdminEmail as fallback")
	}
}

// TestAdminGuardForbidsNonAdmin verifies that withAdminGuard returns 403 for non-admin users.
func TestAdminGuardForbidsNonAdmin(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: "admin@example.com"}}

	called := false
	handler := h.withAdminGuard(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard/admin", nil)
	ctx := context.WithValue(req.Context(), ctxUsername, "regular-user")
	ctx = context.WithValue(ctx, ctxEmail, "user@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if called {
		t.Fatal("inner handler should not have been called")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Access Denied") {
		t.Fatal("expected Access Denied in response body")
	}
}

// TestAdminGuardAllowsAdmin verifies that withAdminGuard passes through for admin users.
func TestAdminGuardAllowsAdmin(t *testing.T) {
	h := &handlers{cfg: DashboardConfig{AdminEmail: "admin@example.com"}}

	called := false
	handler := h.withAdminGuard(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard/admin", nil)
	ctx := context.WithValue(req.Context(), ctxEmail, "admin@example.com")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatal("inner handler should have been called for admin")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestLogoutSetsCorrectCookieAttributes verifies the logout cookie attributes.
func TestLogoutSetsCorrectCookieAttributes(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodPost, "/dashboard/logout", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	cookies := rec.Result().Cookies()
	var found *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("expected engram_session cookie")
	}
	if found.MaxAge >= 0 {
		t.Fatalf("expected negative MaxAge, got %d", found.MaxAge)
	}
	if !found.HttpOnly {
		t.Fatal("expected HttpOnly=true")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected SameSite=Lax, got %d", found.SameSite)
	}
	if found.Path != "/dashboard" {
		t.Fatalf("expected Path=/dashboard, got %s", found.Path)
	}
}

// TestLoginSubmitInvalidFormEncoding verifies graceful handling of bad POST body.
func TestLoginSubmitInvalidFormEncoding(t *testing.T) {
	mux := setupTestMux(t)

	// Send POST with non-form content type — ParseForm may still work but
	// identifier/password will be empty, so we test the required fields path.
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader("not=valid"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render with error), got %d", rec.Code)
	}
}

// TestLoginSubmitPartialFields verifies that submitting only one field shows error.
func TestLoginSubmitPartialFields(t *testing.T) {
	mux := setupTestMux(t)

	form := url.Values{"identifier": {"admin"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (re-render with error), got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "required") {
		t.Fatalf("expected error about required fields, got: %s", body)
	}
}

// TestHealthEndpointResponseJSON verifies that /dashboard/health returns valid JSON.
func TestHealthEndpointResponseJSON(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %s", ct)
	}
}

// TestDashboardHomeComponent verifies DashboardHome renders user-specific content.
func TestDashboardHomeComponent(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := DashboardHome("gentleman")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	// The htmx stats trigger should be present
	if !strings.Contains(body, "hx-get") {
		t.Fatal("expected hx-get attribute in DashboardHome")
	}
	if !strings.Contains(body, "MEMORY FABRIC / OPERATOR gentleman") {
		t.Fatal("expected username-oriented hero copy")
	}
}

// TestBrowserPageSelectedProject verifies the browser page marks selected project.
func TestBrowserPageSelectedProject(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := BrowserPage([]string{"proj-a", "proj-b"}, []string{"bugfix", "decision"}, "proj-a", "search-term", "bugfix")
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "proj-a") {
		t.Fatal("expected proj-a")
	}
	if !strings.Contains(body, "search-term") {
		t.Fatal("expected search-term as value in search input")
	}
	if !strings.Contains(body, "type-pill active") {
		t.Fatal("expected active type pill")
	}
}

// TestSessionDetailPageRendersConnectedData verifies session detail includes observations and prompts.
func TestSessionDetailPageRendersConnectedData(t *testing.T) {
	session := &cloudstore.CloudSession{ID: "sess-1", Project: "engram", Directory: "/tmp/engram", StartedAt: "2025-01-15T10:00:00Z"}
	observations := []cloudstore.CloudObservation{{ID: 11, SessionID: "sess-1", Title: "Fix sync", Type: "bugfix", CreatedAt: "2025-01-15T10:01:00Z"}}
	prompts := []cloudstore.CloudPrompt{{ID: 22, SessionID: "sess-1", Content: "how do we drain backlog?", CreatedAt: "2025-01-15T10:02:00Z"}}

	rec := httptest.NewRecorder()
	comp := SessionDetailPage(session, observations, prompts)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Fix sync") {
		t.Fatal("expected observation in session detail")
	}
	if !strings.Contains(body, "drain backlog") {
		t.Fatal("expected prompt in session detail")
	}
}

// TestObservationDetailPageLinksBackToSession verifies observation detail keeps navigation connected.
func TestObservationDetailPageLinksBackToSession(t *testing.T) {
	obs := &cloudstore.CloudObservation{ID: 7, SessionID: "sess-7", Type: "bugfix", Title: "Payload mismatch", Content: "Full payload body", Scope: "project", CreatedAt: "2025-01-15T10:00:00Z", UpdatedAt: "2025-01-15T10:05:00Z", RevisionCount: 1, DuplicateCount: 1}
	session := &cloudstore.CloudSession{ID: "sess-7", Project: "engram", StartedAt: "2025-01-15T09:55:00Z"}
	related := []cloudstore.CloudObservation{{ID: 8, SessionID: "sess-7", Title: "Repair journal", Type: "decision", CreatedAt: "2025-01-15T10:06:00Z"}}

	rec := httptest.NewRecorder()
	comp := ObservationDetailPage(obs, session, related)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dashboard/sessions/sess-7") {
		t.Fatal("expected link back to session")
	}
	if !strings.Contains(body, "Repair journal") {
		t.Fatal("expected related observation")
	}
}

// TestProjectDetailPageWithAllData verifies project detail renders all data sections.
func TestProjectDetailPageWithAllData(t *testing.T) {
	project := "test-proj"
	stat := &cloudstore.ProjectStat{Project: "test-proj", SessionCount: 1, ObservationCount: 2, PromptCount: 3}
	sessions := []cloudstore.CloudSessionSummary{
		{ID: "s1", Project: "test-proj", StartedAt: "2025-01-15T10:00:00Z"},
	}
	obs := []cloudstore.CloudObservation{
		{ID: 1, Title: "Test obs", Type: "bugfix", CreatedAt: "2025-01-15T10:00:00Z"},
	}
	prompts := []cloudstore.CloudPrompt{
		{ID: 1, Content: "test prompt", CreatedAt: "2025-01-15T10:00:00Z"},
	}

	rec := httptest.NewRecorder()
	comp := ProjectDetailPage(project, stat, nil, sessions, obs, prompts)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "test-proj") {
		t.Fatal("expected project name")
	}
	if !strings.Contains(body, "Test obs") {
		t.Fatal("expected observation title")
	}
	if !strings.Contains(body, "test prompt") {
		t.Fatal("expected prompt content")
	}
}

// TestAdminPageNilHealth verifies admin page handles nil health gracefully.
func TestAdminPageNilHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	comp := AdminPage(nil, nil)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Admin") {
		t.Fatal("expected Admin heading even with nil health")
	}
}

func TestAdminProjectsPageShowsReasonAndUpdater(t *testing.T) {
	rec := httptest.NewRecorder()
	controls := []cloudstore.ProjectSyncControl{{Project: "engram", SyncEnabled: false, PausedReason: strPtr("Security hold"), UpdatedBy: strPtr("gentleman"), UpdatedAt: "2025-01-15T10:00:00Z"}}
	comp := AdminProjectsPage(controls)
	if err := comp.Render(context.Background(), rec); err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Security hold") {
		t.Fatal("expected paused reason")
	}
	if !strings.Contains(body, "gentleman") {
		t.Fatal("expected updater name")
	}
	if !strings.Contains(body, "Reason when pausing") {
		t.Fatal("expected reason input")
	}
}

// TestSessionCookieName verifies the constant value.
func TestSessionCookieName(t *testing.T) {
	if sessionCookieName != "engram_session" {
		t.Fatalf("expected engram_session, got %s", sessionCookieName)
	}
}

// TestTruncateContentEdgeCases verifies additional truncation edge cases.
func TestTruncateContentEdgeCases(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"hello world", 11, "hello world"},  // exactly fits
		{"hello world", 100, "hello world"}, // well under max
		{"ab", 1, "a..."},                   // very small max
	}
	for _, tt := range tests {
		result := truncateContent(tt.input, tt.max)
		if result != tt.expected {
			t.Errorf("truncateContent(%q, %d) = %q, want %q", tt.input, tt.max, result, tt.expected)
		}
	}
}

// TestUnauthenticatedProjectDetailRedirects verifies project detail redirects to login.
func TestUnauthenticatedProjectDetailRedirects(t *testing.T) {
	mux := setupTestMux(t)

	req := httptest.NewRequest(http.MethodGet, "/dashboard/projects/myproject", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	location := rec.Header().Get("Location")
	if location != "/dashboard/login" {
		t.Fatalf("expected redirect to /dashboard/login, got %s", location)
	}
}

// ─── Test Helpers ───────────────────────────────────────────────────────────

// setupTestMux creates a mux with dashboard routes mounted for testing.
// It uses nil store and authSvc since most tests don't need actual auth.
func setupTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	// Pass nil store — tests that need it will create a real one.
	// authSvc is nil — the cookie middleware will fail validation, which is
	// what we want for testing unauthenticated redirects.
	Mount(mux, nil, nil, DashboardConfig{})
	return mux
}

// readBody is a test helper that reads and returns the response body as string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	return string(b)
}
