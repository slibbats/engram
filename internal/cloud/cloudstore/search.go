package cloudstore

import (
	"fmt"
	"regexp"
	"strings"
)

// ─── Search Types ───────────────────────────────────────────────────────────

// CloudSearchOptions configures observation search filters.
type CloudSearchOptions struct {
	Type    string // Filter by observation type (e.g. "decision", "note")
	Project string // Filter by project name
	Scope   string // Filter by scope ("project", "personal", "global")
	Limit   int    // Max results to return (default 20)
}

// CloudSearchResult holds a single search result with relevance ranking.
type CloudSearchResult struct {
	CloudObservation
	Rank float64 `json:"rank"` // ts_rank_cd score
}

// ObservationTypes returns the distinct observation types currently present for
// a user, optionally filtered by project, ordered by frequency and then name.
func (cs *CloudStore) ObservationTypes(userID, project string) ([]string, error) {
	query := `
		SELECT type
		FROM cloud_observations
		WHERE user_id = $1 AND deleted_at IS NULL AND type <> ''
	`
	args := []any{userID}
	argN := 2

	if project != "" {
		query += fmt.Sprintf(" AND project = $%d", argN)
		args = append(args, project)
		argN++
	}

	query += ` GROUP BY type ORDER BY COUNT(*) DESC, type ASC`

	rows, err := cs.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("cloudstore: observation types: %w", err)
	}
	defer rows.Close()

	var types []string
	for rows.Next() {
		var obsType string
		if err := rows.Scan(&obsType); err != nil {
			return nil, fmt.Errorf("cloudstore: scan observation type: %w", err)
		}
		types = append(types, obsType)
	}
	return types, rows.Err()
}

// ─── Query Sanitization ─────────────────────────────────────────────────────

// safeCharsRE strips everything except letters, digits, spaces, and the
// asterisk used for prefix matching. This prevents tsquery parse errors
// from special characters like quotes, colons, parentheses, etc.
var safeCharsRE = regexp.MustCompile(`[^\p{L}\p{N}\s*]+`)

// sanitizeQuery converts raw user input into a safe string by stripping
// special characters, collapsing whitespace, and trimming boundaries.
func sanitizeQuery(raw string) string {
	cleaned := safeCharsRE.ReplaceAllString(raw, " ")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return strings.TrimSpace(cleaned)
}

func buildTSQuery(raw string) string {
	safe := sanitizeQuery(raw)
	if safe == "" {
		return ""
	}

	terms := strings.Fields(safe)
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSuffix(term, "*")
		if term == "" {
			continue
		}
		parts = append(parts, term+":*")
	}

	return strings.Join(parts, " & ")
}

// ─── Observation Search ─────────────────────────────────────────────────────

// Search performs a full-text search over cloud_observations for the given
// user. Results are ranked by ts_rank_cd using the tsv GENERATED STORED
// column. Title matches (weight A) rank higher than content matches (B)
// which rank higher than type/project matches (C).
//
// The query is processed by plainto_tsquery('english', $N) which safely
// handles multi-word input by ANDing terms. All filtering uses
// parameterized placeholders -- no string concatenation touches SQL.
//
// Covers: CLOUD-SRV-06, FTS-01, FTS-03, NFR-01, NFR-02.
func (cs *CloudStore) Search(userID, query string, opts CloudSearchOptions) ([]CloudSearchResult, error) {
	tsQuery := buildTSQuery(query)
	if tsQuery == "" {
		return []CloudSearchResult{}, nil
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	// Build query with parameterized placeholders.
	// $1 = userID, $2 = search query (for to_tsquery)
	sql := `
		SELECT id, user_id, session_id, type, title, content, tool_name, project,
		       scope, topic_key, revision_count, duplicate_count, last_seen_at,
		       created_at, updated_at, deleted_at,
		       ts_rank_cd(tsv, to_tsquery('english', $2)) AS rank
		FROM cloud_observations
		WHERE user_id = $1
		  AND deleted_at IS NULL
		  AND tsv @@ to_tsquery('english', $2)
	`
	args := []any{userID, tsQuery}
	argN := 3

	if opts.Type != "" {
		sql += fmt.Sprintf(" AND type = $%d", argN)
		args = append(args, opts.Type)
		argN++
	}
	if opts.Project != "" {
		sql += fmt.Sprintf(" AND project = $%d", argN)
		args = append(args, opts.Project)
		argN++
	}
	if opts.Scope != "" {
		sql += fmt.Sprintf(" AND scope = $%d", argN)
		args = append(args, normalizeScope(opts.Scope))
		argN++
	}

	sql += fmt.Sprintf(" ORDER BY rank DESC, created_at DESC LIMIT $%d", argN)
	args = append(args, limit)

	rows, err := cs.db.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cloudstore: search: %w", err)
	}
	defer rows.Close()

	var results []CloudSearchResult
	for rows.Next() {
		var r CloudSearchResult
		if err := rows.Scan(
			&r.ID, &r.UserID, &r.SessionID, &r.Type, &r.Title, &r.Content,
			&r.ToolName, &r.Project, &r.Scope, &r.TopicKey,
			&r.RevisionCount, &r.DuplicateCount, &r.LastSeenAt,
			&r.CreatedAt, &r.UpdatedAt, &r.DeletedAt,
			&r.Rank,
		); err != nil {
			return nil, fmt.Errorf("cloudstore: scan search result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cloudstore: search rows: %w", err)
	}

	// Guarantee non-nil slice for JSON serialization.
	if results == nil {
		results = []CloudSearchResult{}
	}
	return results, nil
}

// ─── Prompt Search ──────────────────────────────────────────────────────────

// SearchPrompts performs a full-text search over cloud_prompts for the given
// user. It uses the tsv GENERATED STORED column with plainto_tsquery.
// Results are ordered by rank descending, then created_at descending.
//
// Covers: FTS-02.
func (cs *CloudStore) SearchPrompts(userID, query, project string, limit int) ([]CloudPrompt, error) {
	tsQuery := buildTSQuery(query)
	if tsQuery == "" {
		return []CloudPrompt{}, nil
	}

	if limit <= 0 {
		limit = 20
	}

	sql := `
		SELECT id, user_id, session_id, content, COALESCE(project, '') as project, created_at
		FROM cloud_prompts
		WHERE user_id = $1
		  AND tsv @@ to_tsquery('english', $2)
	`
	args := []any{userID, tsQuery}
	argN := 3

	if project != "" {
		sql += fmt.Sprintf(" AND project = $%d", argN)
		args = append(args, project)
		argN++
	}

	sql += fmt.Sprintf(" ORDER BY ts_rank_cd(tsv, to_tsquery('english', $2)) DESC, created_at DESC LIMIT $%d", argN)
	args = append(args, limit)

	rows, err := cs.db.Query(sql, args...)
	if err != nil {
		return nil, fmt.Errorf("cloudstore: search prompts: %w", err)
	}
	defer rows.Close()

	var results []CloudPrompt
	for rows.Next() {
		var p CloudPrompt
		if err := rows.Scan(&p.ID, &p.UserID, &p.SessionID, &p.Content, &p.Project, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("cloudstore: scan prompt search result: %w", err)
		}
		results = append(results, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cloudstore: search prompts rows: %w", err)
	}

	if results == nil {
		results = []CloudPrompt{}
	}
	return results, nil
}
