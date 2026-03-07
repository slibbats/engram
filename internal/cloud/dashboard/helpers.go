package dashboard

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
)

func totalSessionCount(stats []cloudstore.ProjectStat) int {
	total := 0
	for _, stat := range stats {
		total += stat.SessionCount
	}
	return total
}

func totalObservationCount(stats []cloudstore.ProjectStat) int {
	total := 0
	for _, stat := range stats {
		total += stat.ObservationCount
	}
	return total
}

func totalPromptCount(stats []cloudstore.ProjectStat) int {
	total := 0
	for _, stat := range stats {
		total += stat.PromptCount
	}
	return total
}

func browserURL(project, search, obsType string) string {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if search != "" {
		v.Set("q", search)
	}
	if obsType != "" {
		v.Set("type", obsType)
	}
	if encoded := v.Encode(); encoded != "" {
		return fmt.Sprintf("/dashboard/browser?%s", encoded)
	}
	return "/dashboard/browser"
}

func typePillClass(activeType, candidate string) string {
	if activeType == candidate {
		return "type-pill active"
	}
	if activeType == "" && candidate == "" {
		return "type-pill active"
	}
	return "type-pill"
}

func formatTimestamp(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return "-"
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	return parsed.Local().Format("02 Jan 2006 15:04")
}

func formatTimestampPtr(ts *string) string {
	if ts == nil {
		return "Never"
	}
	return formatTimestamp(*ts)
}

func countPausedProjects(controls []cloudstore.ProjectSyncControl) int {
	count := 0
	for _, control := range controls {
		if !control.SyncEnabled {
			count++
		}
	}
	return count
}

func controlsByProject(controls []cloudstore.ProjectSyncControl) map[string]cloudstore.ProjectSyncControl {
	indexed := make(map[string]cloudstore.ProjectSyncControl, len(controls))
	for _, control := range controls {
		indexed[control.Project] = control
	}
	return indexed
}

func projectControlReasonValue(control cloudstore.ProjectSyncControl) string {
	if control.PausedReason == nil {
		return ""
	}
	return *control.PausedReason
}

func projectControl(controls map[string]cloudstore.ProjectSyncControl, project string) cloudstore.ProjectSyncControl {
	if controls == nil {
		return cloudstore.ProjectSyncControl{Project: project, SyncEnabled: true}
	}
	control, ok := controls[project]
	if !ok {
		return cloudstore.ProjectSyncControl{Project: project, SyncEnabled: true}
	}
	return control
}

// truncateContent truncates a string to max runes, appending "..." if needed.
func truncateContent(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "..."
}

var structuredFieldRE = regexp.MustCompile(`\*\*(What|Why|Where|Learned)\*\*:\s*`)
var headingSectionRE = regexp.MustCompile(`(?m)^##\s+([^\n#]+?)\s*$`)

func renderStructuredContent(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "<p class=\"muted\">No content captured.</p>"
	}
	if !structuredFieldRE.MatchString(raw) {
		if headingSectionRE.MatchString(raw) {
			return renderHeadingSections(raw)
		}
		return renderParagraphBlocks(raw)
	}

	parts := structuredFieldRE.Split(raw, -1)
	matches := structuredFieldRE.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return renderParagraphBlocks(raw)
	}

	var b strings.Builder
	b.WriteString(`<div class="structured-content">`)
	for i, match := range matches {
		if len(match) < 2 {
			continue
		}
		value := ""
		if i+1 < len(parts) {
			value = strings.TrimSpace(parts[i+1])
		}
		if value == "" {
			continue
		}
		b.WriteString(`<section class="structured-block">`)
		b.WriteString(`<h4>` + html.EscapeString(match[1]) + `</h4>`)
		b.WriteString(renderParagraphBlocks(value))
		b.WriteString(`</section>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func renderInlineStructuredPreview(raw string, max int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "No content captured."
	}
	if structuredFieldRE.MatchString(raw) {
		labels := structuredFieldRE.FindAllStringSubmatch(raw, -1)
		parts := structuredFieldRE.Split(raw, -1)
		chunks := make([]string, 0, len(labels))
		for i, label := range labels {
			if len(label) < 2 {
				continue
			}
			value := ""
			if i+1 < len(parts) {
				value = strings.TrimSpace(parts[i+1])
			}
			if value == "" {
				continue
			}
			chunks = append(chunks, label[1]+": "+normalizeWhitespace(value))
		}
		raw = strings.Join(chunks, " • ")
	} else if headingSectionRE.MatchString(raw) {
		raw = flattenHeadingSections(raw)
	}
	return truncateContent(normalizeWhitespace(raw), max)
}

func renderHeadingSections(raw string) string {
	matches := headingSectionRE.FindAllStringSubmatchIndex(raw, -1)
	if len(matches) == 0 {
		return renderParagraphBlocks(raw)
	}
	var b strings.Builder
	b.WriteString(`<div class="structured-content">`)
	for i, match := range matches {
		label := strings.TrimSpace(raw[match[2]:match[3]])
		start := match[1]
		end := len(raw)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		value := strings.TrimSpace(raw[start:end])
		if value == "" {
			continue
		}
		b.WriteString(`<section class="structured-block">`)
		b.WriteString(`<h4>` + html.EscapeString(label) + `</h4>`)
		b.WriteString(renderParagraphBlocks(value))
		b.WriteString(`</section>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func flattenHeadingSections(raw string) string {
	matches := headingSectionRE.FindAllStringSubmatchIndex(raw, -1)
	if len(matches) == 0 {
		return raw
	}
	chunks := make([]string, 0, len(matches))
	for i, match := range matches {
		label := strings.TrimSpace(raw[match[2]:match[3]])
		start := match[1]
		end := len(raw)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		value := strings.TrimSpace(raw[start:end])
		if value == "" {
			continue
		}
		chunks = append(chunks, label+": "+normalizeWhitespace(value))
	}
	return strings.Join(chunks, " • ")
}

func renderParagraphBlocks(raw string) string {
	blocks := strings.Split(strings.TrimSpace(raw), "\n\n")
	var b strings.Builder
	for _, block := range blocks {
		text := strings.TrimSpace(block)
		if text == "" {
			continue
		}
		b.WriteString(`<p>` + html.EscapeString(normalizeWhitespacePreserveParagraph(text)) + `</p>`)
	}
	if b.Len() == 0 {
		return `<p class="muted">No content captured.</p>`
	}
	return b.String()
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeWhitespacePreserveParagraph(s string) string {
	lines := strings.Split(s, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, " ")
}

// typeBadgeVariant returns a badge color variant for an observation type.
func typeBadgeVariant(obsType string) string {
	switch obsType {
	case "decision", "architecture":
		return "success"
	case "bugfix":
		return "danger"
	case "discovery", "learning":
		return "warning"
	default:
		return "muted"
	}
}
