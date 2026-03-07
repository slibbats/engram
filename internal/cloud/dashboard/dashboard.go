// Package dashboard provides a server-rendered web UI for Engram Cloud.
// It uses templ for HTML templating and htmx for partial page updates.
// All static assets are embedded in the binary via go:embed.
package dashboard

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"

	"github.com/Gentleman-Programming/engram/internal/cloud/auth"
	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
)

// Mount registers all dashboard routes on the given mux. It attaches:
//   - GET /dashboard/static/* — embedded static files (htmx, CSS)
//   - GET /dashboard/health   — dashboard health check
//   - GET /dashboard/login    — login page (unauthenticated)
//   - POST /dashboard/login   — login form submission
//   - POST /dashboard/logout  — clear session cookie
//   - GET /dashboard/         — main dashboard (authenticated)
//   - GET /dashboard/stats    — dashboard stats partial (htmx)
//   - GET /dashboard/browser  — knowledge browser page
//   - GET /dashboard/browser/observations — observations partial (htmx)
//   - GET /dashboard/browser/sessions     — sessions partial (htmx)
//   - GET /dashboard/browser/prompts      — prompts partial (htmx)
//   - GET /dashboard/projects       — projects list
//   - GET /dashboard/projects/{name} — project detail
//   - GET /dashboard/contributors   — contributors list
//   - GET /dashboard/admin          — admin overview (admin only)
//   - GET /dashboard/admin/users    — admin user management (admin only)
//   - GET /dashboard/admin/health   — admin system health (admin only)
func Mount(mux *http.ServeMux, store *cloudstore.CloudStore, authSvc *auth.Service, cfg DashboardConfig) {
	h := &handlers{
		store:   store,
		authSvc: authSvc,
		cfg:     cfg,
	}

	// Static assets — strip the /dashboard/static/ prefix so the embed.FS
	// paths resolve correctly (embed.FS root is "static/").
	staticSub, err := fs.Sub(StaticFS, "static")
	if err != nil {
		log.Fatalf("dashboard: failed to create sub-FS for static assets: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /dashboard/static/", http.StripPrefix("/dashboard/static/", fileServer))

	// Health check (no auth)
	mux.HandleFunc("GET /dashboard/health", h.handleHealth)

	// Login (no auth)
	mux.HandleFunc("GET /dashboard/login", h.handleLoginPage)
	mux.HandleFunc("POST /dashboard/login", h.handleLoginSubmit)

	// Logout (no auth — clearing cookie is safe)
	mux.HandleFunc("POST /dashboard/logout", h.handleLogout)

	// ── Authenticated routes ─────────────────────────────────────────────

	// Phase 4: Dashboard overview
	mux.HandleFunc("GET /dashboard/", withCookieAuth(authSvc, h.handleDashboard))
	mux.HandleFunc("GET /dashboard", withCookieAuth(authSvc, h.handleDashboardRedirect))
	mux.HandleFunc("GET /dashboard/stats", withCookieAuth(authSvc, h.handleDashboardStats))

	// Phase 5: Knowledge browser
	mux.HandleFunc("GET /dashboard/browser", withCookieAuth(authSvc, h.handleBrowser))
	mux.HandleFunc("GET /dashboard/browser/observations", withCookieAuth(authSvc, h.handleBrowserObservations))
	mux.HandleFunc("GET /dashboard/browser/sessions", withCookieAuth(authSvc, h.handleBrowserSessions))
	mux.HandleFunc("GET /dashboard/browser/prompts", withCookieAuth(authSvc, h.handleBrowserPrompts))
	mux.HandleFunc("GET /dashboard/sessions/{id}", withCookieAuth(authSvc, h.handleSessionDetail))
	mux.HandleFunc("GET /dashboard/observations/{id}", withCookieAuth(authSvc, h.handleObservationDetail))
	mux.HandleFunc("GET /dashboard/prompts/{id}", withCookieAuth(authSvc, h.handlePromptDetail))

	// Phase 6: Projects
	mux.HandleFunc("GET /dashboard/projects", withCookieAuth(authSvc, h.handleProjects))
	mux.HandleFunc("GET /dashboard/projects/{name}", withCookieAuth(authSvc, h.handleProjectDetail))

	// Phase 7: Contributors
	mux.HandleFunc("GET /dashboard/contributors", withCookieAuth(authSvc, h.handleContributors))
	mux.HandleFunc("GET /dashboard/contributors/{id}", withCookieAuth(authSvc, h.handleContributorDetail))

	// Phase 8: Admin (admin guard applied inside handlers)
	mux.HandleFunc("GET /dashboard/admin", withCookieAuth(authSvc, h.withAdminGuard(h.handleAdmin)))
	mux.HandleFunc("GET /dashboard/admin/projects", withCookieAuth(authSvc, h.withAdminGuard(h.handleAdminProjects)))
	mux.HandleFunc("POST /dashboard/admin/projects/{name}/sync", withCookieAuth(authSvc, h.withAdminGuard(h.handleAdminProjectSyncToggle)))
	mux.HandleFunc("GET /dashboard/admin/users", withCookieAuth(authSvc, h.withAdminGuard(h.handleAdminUsers)))
	mux.HandleFunc("GET /dashboard/admin/health", withCookieAuth(authSvc, h.withAdminGuard(h.handleAdminHealth)))
}

// handlers groups all dashboard HTTP handlers and their dependencies.
type handlers struct {
	store   *cloudstore.CloudStore
	authSvc *auth.Service
	cfg     DashboardConfig
}

// handleHealth returns a simple health check for the dashboard subsystem.
func (h *handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","subsystem":"dashboard"}`))
}

// handleDashboardRedirect redirects /dashboard to /dashboard/ for consistent routing.
func (h *handlers) handleDashboardRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
}

// ─── Phase 4: Dashboard Overview ────────────────────────────────────────────

// handleDashboard renders the main dashboard page.
func (h *handlers) handleDashboard(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)

	content := DashboardHome(username)
	page := Layout("Dashboard", username, "dashboard", isAdmin, content)
	page.Render(r.Context(), w)
}

// handleDashboardStats returns the project stats partial (htmx).
func (h *handlers) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)

	var stats []cloudstore.ProjectStat
	var projects []string
	if h.store != nil {
		stats, _ = h.store.ProjectStats(userID)
		projects, _ = h.store.UserProjects(userID)
	}

	DashboardStatsPartial(stats, projects).Render(r.Context(), w)
}

// ─── Phase 5: Knowledge Browser ─────────────────────────────────────────────

// handleBrowser renders the knowledge browser page.
func (h *handlers) handleBrowser(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)
	activeProject := r.URL.Query().Get("project")
	activeType := r.URL.Query().Get("type")
	search := r.URL.Query().Get("q")

	var (
		projects []string
		types    []string
	)
	if h.store != nil {
		projects, _ = h.store.UserProjects(userID)
		types, _ = h.store.ObservationTypes(userID, activeProject)
	}

	content := BrowserPage(projects, types, activeProject, search, activeType)
	page := Layout("Browser", username, "browser", isAdmin, content)
	page.Render(r.Context(), w)
}

// handleBrowserObservations returns the observations partial (htmx).
func (h *handlers) handleBrowserObservations(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	project := r.URL.Query().Get("project")
	search := r.URL.Query().Get("q")
	obsType := r.URL.Query().Get("type")

	var observations []cloudstore.CloudObservation
	if h.store != nil {
		if search != "" {
			results, _ := h.store.Search(userID, search, cloudstore.CloudSearchOptions{
				Type:    obsType,
				Project: project,
				Limit:   50,
			})
			for _, sr := range results {
				observations = append(observations, sr.CloudObservation)
			}
		} else {
			observations, _ = h.store.FilterObservations(userID, project, "", obsType, 50)
		}
	}

	ObservationsPartial(observations).Render(r.Context(), w)
}

// handleBrowserSessions returns the sessions partial (htmx).
func (h *handlers) handleBrowserSessions(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	project := r.URL.Query().Get("project")

	var sessions []cloudstore.CloudSessionSummary
	if h.store != nil {
		sessions, _ = h.store.RecentSessions(userID, project, 50)
	}

	SessionsPartial(sessions).Render(r.Context(), w)
}

// handleBrowserPrompts returns the prompts partial (htmx).
func (h *handlers) handleBrowserPrompts(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	project := r.URL.Query().Get("project")
	search := r.URL.Query().Get("q")

	var prompts []cloudstore.CloudPrompt
	if h.store != nil {
		if search != "" {
			prompts, _ = h.store.SearchPrompts(userID, search, project, 50)
		} else {
			prompts, _ = h.store.RecentPrompts(userID, project, 50)
		}
	}

	PromptsPartial(prompts).Render(r.Context(), w)
}

// ─── Phase 6: Projects ──────────────────────────────────────────────────────

// handleProjects renders the projects list view.
func (h *handlers) handleProjects(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)

	var stats []cloudstore.ProjectStat
	controlMap := map[string]cloudstore.ProjectSyncControl{}
	if h.store != nil {
		stats, _ = h.store.ProjectStats(userID)
		controls, _ := h.store.ListProjectSyncControls()
		controlMap = controlsByProject(controls)
	}

	content := ProjectsPage(stats, controlMap)
	page := Layout("Projects", username, "projects", isAdmin, content)
	page.Render(r.Context(), w)
}

// handleProjectDetail renders the detail view for a single project.
func (h *handlers) handleProjectDetail(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)
	projectName := r.PathValue("name")

	var (
		projectStat  *cloudstore.ProjectStat
		control      *cloudstore.ProjectSyncControl
		sessions     []cloudstore.CloudSessionSummary
		observations []cloudstore.CloudObservation
		prompts      []cloudstore.CloudPrompt
	)

	if h.store != nil {
		// Get project-specific stats
		allStats, _ := h.store.ProjectStats(userID)
		for i := range allStats {
			if allStats[i].Project == projectName {
				projectStat = &allStats[i]
				break
			}
		}

		sessions, _ = h.store.RecentSessions(userID, projectName, 20)
		observations, _ = h.store.RecentObservations(userID, projectName, "", 20)
		prompts, _ = h.store.RecentPrompts(userID, projectName, 20)
		control, _ = h.store.GetProjectSyncControl(projectName)
	}

	content := ProjectDetailPage(projectName, projectStat, control, sessions, observations, prompts)
	page := Layout(projectName, username, "projects", isAdmin, content)
	page.Render(r.Context(), w)
}

// handleSessionDetail renders a connected view for a single session.
func (h *handlers) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)
	sessionID := r.PathValue("id")

	var (
		session      *cloudstore.CloudSession
		observations []cloudstore.CloudObservation
		prompts      []cloudstore.CloudPrompt
	)
	if h.store != nil {
		session, _ = h.store.GetSession(userID, sessionID)
		if session != nil {
			observations, _ = h.store.SessionObservations(userID, sessionID, 200)
			prompts, _ = h.store.SessionPrompts(userID, sessionID, 200)
		}
	}

	if session == nil {
		http.NotFound(w, r)
		return
	}

	content := SessionDetailPage(session, observations, prompts)
	page := Layout(session.Project+" Session", username, "browser", isAdmin, content)
	page.Render(r.Context(), w)
}

// handleObservationDetail renders the full detail page for a single observation.
func (h *handlers) handleObservationDetail(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)

	var obsID int64
	if _, err := fmt.Sscan(r.PathValue("id"), &obsID); err != nil {
		http.NotFound(w, r)
		return
	}

	var (
		observation *cloudstore.CloudObservation
		session     *cloudstore.CloudSession
		related     []cloudstore.CloudObservation
	)
	if h.store != nil {
		observation, _ = h.store.GetObservation(userID, obsID)
		if observation != nil {
			session, _ = h.store.GetSession(userID, observation.SessionID)
			related, _ = h.store.SessionObservations(userID, observation.SessionID, 200)
			filtered := make([]cloudstore.CloudObservation, 0, len(related))
			for _, candidate := range related {
				if candidate.ID == observation.ID {
					continue
				}
				filtered = append(filtered, candidate)
			}
			related = filtered
		}
	}

	if observation == nil {
		http.NotFound(w, r)
		return
	}

	content := ObservationDetailPage(observation, session, related)
	page := Layout(observation.Title, username, "browser", isAdmin, content)
	page.Render(r.Context(), w)
}

// ─── Phase 7: Contributors ──────────────────────────────────────────────────

// handleContributors renders the contributors list view.
func (h *handlers) handleContributors(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)

	var contributors []cloudstore.ContributorStat
	if h.store != nil {
		contributors, _ = h.store.ContributorStats()
	}

	content := ContributorsPage(contributors)
	page := Layout("Contributors", username, "contributors", isAdmin, content)
	page.Render(r.Context(), w)
}

func (h *handlers) handlePromptDetail(w http.ResponseWriter, r *http.Request) {
	userID := getUserIDFromContext(r)
	username := getUsernameFromContext(r)
	isAdmin := h.isAdmin(r)

	var promptID int64
	if _, err := fmt.Sscan(r.PathValue("id"), &promptID); err != nil {
		http.NotFound(w, r)
		return
	}

	var (
		prompt  *cloudstore.CloudPrompt
		session *cloudstore.CloudSession
		related []cloudstore.CloudPrompt
	)
	if h.store != nil {
		prompt, _ = h.store.GetPrompt(userID, promptID)
		if prompt != nil {
			session, _ = h.store.GetSession(userID, prompt.SessionID)
			related, _ = h.store.SessionPrompts(userID, prompt.SessionID, 200)
			filtered := make([]cloudstore.CloudPrompt, 0, len(related))
			for _, candidate := range related {
				if candidate.ID == prompt.ID {
					continue
				}
				filtered = append(filtered, candidate)
			}
			related = filtered
		}
	}
	if prompt == nil {
		http.NotFound(w, r)
		return
	}

	content := PromptDetailPage(prompt, session, related)
	page := Layout("Prompt", username, "browser", isAdmin, content)
	page.Render(r.Context(), w)
}

func (h *handlers) handleContributorDetail(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)

	contributorID := r.PathValue("id")
	var (
		user         *cloudstore.CloudUser
		contributor  *cloudstore.ContributorStat
		sessions     []cloudstore.CloudSessionSummary
		observations []cloudstore.CloudObservation
		prompts      []cloudstore.CloudPrompt
	)
	if h.store != nil {
		user, _ = h.store.GetUserByID(contributorID)
		if user != nil {
			stats, _ := h.store.ContributorStats()
			for i := range stats {
				if stats[i].UserID == contributorID {
					contributor = &stats[i]
					break
				}
			}
			sessions, _ = h.store.RecentSessions(contributorID, "", 20)
			observations, _ = h.store.RecentObservations(contributorID, "", "", 20)
			prompts, _ = h.store.RecentPrompts(contributorID, "", 20)
		}
	}
	if user == nil {
		http.NotFound(w, r)
		return
	}

	content := ContributorDetailPage(user, contributor, sessions, observations, prompts)
	page := Layout("Contributor", username, "contributors", true, content)
	page.Render(r.Context(), w)
}

// ─── Phase 8: Admin Views ───────────────────────────────────────────────────

// handleAdmin renders the admin overview page.
func (h *handlers) handleAdmin(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)

	var (
		health   *cloudstore.SystemHealthInfo
		controls []cloudstore.ProjectSyncControl
	)
	if h.store != nil {
		health, _ = h.store.SystemHealth()
		controls, _ = h.store.ListProjectSyncControls()
	}

	content := AdminPage(health, controls)
	page := Layout("Admin", username, "admin", true, content)
	page.Render(r.Context(), w)
}

// handleAdminProjects renders cloud-managed project sync controls.
func (h *handlers) handleAdminProjects(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)

	var controls []cloudstore.ProjectSyncControl
	if h.store != nil {
		controls, _ = h.store.ListProjectSyncControls()
	}

	content := AdminProjectsPage(controls)
	page := Layout("Admin — Projects", username, "admin", true, content)
	page.Render(r.Context(), w)
}

// handleAdminProjectSyncToggle updates cloud-managed sync state for a project.
func (h *handlers) handleAdminProjectSyncToggle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/dashboard/admin/projects", http.StatusSeeOther)
		return
	}
	project := r.PathValue("name")
	enabled := r.FormValue("enabled") != "false"
	reason := r.FormValue("reason")
	if h.store != nil {
		_ = h.store.SetProjectSyncEnabled(project, enabled, getUserIDFromContext(r), reason)
	}
	http.Redirect(w, r, "/dashboard/admin/projects", http.StatusSeeOther)
}

// handleAdminUsers renders the user management view.
func (h *handlers) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)

	var users []cloudstore.CloudUser
	if h.store != nil {
		users, _ = h.store.ListAllUsers()
	}

	content := AdminUsersPage(users)
	page := Layout("Admin — Users", username, "admin", true, content)
	page.Render(r.Context(), w)
}

// handleAdminHealth renders the system health detail page.
func (h *handlers) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	username := getUsernameFromContext(r)

	var health *cloudstore.SystemHealthInfo
	if h.store != nil {
		health, _ = h.store.SystemHealth()
	}

	content := AdminHealthPage(health)
	page := Layout("Admin — Health", username, "admin", true, content)
	page.Render(r.Context(), w)
}

// ─── Login & Logout ─────────────────────────────────────────────────────────

// handleLoginPage renders the login form.
func (h *handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	page := LoginPage("")
	page.Render(r.Context(), w)
}

// handleLoginSubmit processes the login form submission.
func (h *handlers) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		page := LoginPage("Invalid form submission")
		page.Render(r.Context(), w)
		return
	}

	identifier := r.FormValue("identifier")
	password := r.FormValue("password")

	if identifier == "" || password == "" {
		page := LoginPage("Username/email and password are required")
		page.Render(r.Context(), w)
		return
	}

	result, err := h.authSvc.Login(identifier, password)
	if err != nil {
		page := LoginPage("Invalid credentials")
		page.Render(r.Context(), w)
		return
	}

	// Set the session cookie with the access token.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    result.AccessToken,
		Path:     "/dashboard",
		MaxAge:   result.ExpiresIn,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
}

// handleLogout clears the session cookie and redirects to login.
func (h *handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/dashboard",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// isAdmin checks if the current authenticated user is an admin.
// Admin is determined by matching the ENGRAM_CLOUD_ADMIN email against
// the user's email from context.
func (h *handlers) isAdmin(r *http.Request) bool {
	if h.cfg.AdminEmail == "" {
		return false
	}
	email := getEmailFromContext(r)
	if email != "" && email == h.cfg.AdminEmail {
		return true
	}
	// Fallback: check if username matches the admin email (when email not in JWT).
	username := getUsernameFromContext(r)
	return username == h.cfg.AdminEmail
}

// withAdminGuard wraps a handler to require admin privileges.
// Returns 403 with a forbidden page if the user is not an admin.
func (h *handlers) withAdminGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isAdmin(r) {
			username := getUsernameFromContext(r)
			content := AdminForbidden()
			page := Layout("Access Denied", username, "admin", false, content)
			w.WriteHeader(http.StatusForbidden)
			page.Render(r.Context(), w)
			return
		}
		next(w, r)
	}
}
