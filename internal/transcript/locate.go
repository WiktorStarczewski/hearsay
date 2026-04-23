package transcript

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionSummary is the metadata surface returned by list_sessions and
// get_current_session. It is also consumed by the auto-routing heuristics
// on the caller side (isLive + firstUserMessage drive disambiguation).
type SessionSummary struct {
	SessionID        string    `json:"sessionId"`
	Project          string    `json:"project"`
	Cwd              string    `json:"cwd"`
	GitBranch        string    `json:"gitBranch,omitempty"`
	StartedAt        string    `json:"startedAt,omitempty"`
	LastActivityAt   string    `json:"lastActivityAt"`
	IsLive           bool      `json:"isLive"`
	FirstUserMessage string    `json:"firstUserMessage,omitempty"`
	TurnCount        int       `json:"turnCount"`
	SizeBytes        int64     `json:"sizeBytes"`
	HasSubagents     bool      `json:"hasSubagents"`
	lastActivityTime time.Time `json:"-"`
}

// LocateOptions filter and cap the results of listing sessions.
type LocateOptions struct {
	// DataDir defaults to "~/.claude" when empty.
	DataDir          string
	// LiveWindow is the duration a session can go untouched and still
	// be considered "live" (isLive: true). Defaults to 5 minutes.
	LiveWindow       time.Duration
	// Project filter: substring match against the tail segment of the
	// decoded cwd, or exact match against the full decoded cwd.
	Project          string
	// Since filters out sessions with LastActivityAt < Since.
	Since            time.Time
	// Limit caps the returned list. Zero means no limit.
	Limit            int
}

// ProjectsDir returns the ~/.claude/projects directory.
func ProjectsDir(dataDir string) string {
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dataDir, "projects")
}

// decodeProjectDirName produces a display-friendly approximation of the
// original cwd from the dir name. Claude Code encodes the cwd by replacing
// path separators with hyphens, which is lossy (projects with hyphens in
// their names — e.g. `miden-wallet` — can't be distinguished from
// sub-path boundaries). We only use the result for display; project
// filtering uses the raw dir name via projectDirMatches to keep match
// semantics literal.
func decodeProjectDirName(name string) string {
	return strings.ReplaceAll(name, "-", "/")
}

// projectShortName is a best-effort display name: the tail component of
// the decoded cwd. May mis-split projects with hyphens — consumers should
// not use this for equality checks.
func projectShortName(cwd string) string {
	segs := strings.Split(cwd, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] != "" {
			return segs[i]
		}
	}
	return cwd
}

// projectDirMatches is the authoritative filter for the list_sessions
// project parameter. We substring-match against the raw directory name
// because it literally contains the project name (e.g.
// `-Users-celrisen-miden-miden-wallet` contains `miden-wallet`), which
// sidesteps the hyphen-ambiguity that decodeProjectDirName introduces.
// Full paths are matched against the lossy decoded cwd as a fallback —
// if the caller passes a path, they accept whatever the decoder gives.
func projectDirMatches(filter, rawDirName, decodedCwd string) bool {
	if filter == "" {
		return true
	}
	if strings.Contains(rawDirName, filter) {
		return true
	}
	if strings.HasPrefix(filter, "/") && decodedCwd == filter {
		return true
	}
	return false
}

// ListSessions scans the Claude Code projects directory and returns the
// matching sessions, sorted by most-recent activity first.
func ListSessions(opts LocateOptions) []SessionSummary {
	if opts.LiveWindow == 0 {
		opts.LiveWindow = 5 * time.Minute
	}
	root := ProjectsDir(opts.DataDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	now := time.Now()
	var results []SessionSummary

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rawName := entry.Name()
		decodedCwd := decodeProjectDirName(rawName)
		shortName := projectShortName(decodedCwd)

		if !projectDirMatches(opts.Project, rawName, decodedCwd) {
			continue
		}

		projectDir := filepath.Join(root, entry.Name())
		sessionEntries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}

		for _, se := range sessionEntries {
			name := se.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			sessionID := strings.TrimSuffix(name, ".jsonl")
			jsonlPath := filepath.Join(projectDir, name)

			info, err := se.Info()
			if err != nil {
				continue
			}
			mtime := info.ModTime()

			if !opts.Since.IsZero() && mtime.Before(opts.Since) {
				continue
			}

			summary := buildSummary(jsonlPath, sessionID, decodedCwd, shortName, info.Size(), mtime, now, opts.LiveWindow, projectDir)
			if summary != nil {
				results = append(results, *summary)
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].lastActivityTime.After(results[j].lastActivityTime)
	})

	if opts.Limit > 0 && len(results) > opts.Limit {
		results = results[:opts.Limit]
	}
	return results
}

func buildSummary(
	jsonlPath, sessionID, cwd, shortName string,
	sizeBytes int64,
	mtime time.Time,
	now time.Time,
	liveWindow time.Duration,
	projectDir string,
) *SessionSummary {
	// Parse is cheap relative to the network hop a caller would pay, but
	// we deliberately skip expensive per-session work (no full content
	// scans, no search indexing).
	parsed, err := ParseFile(jsonlPath)
	if err != nil {
		return nil
	}

	var startedAt, gitBranch string
	for i := range parsed.Events {
		e := &parsed.Events[i]
		if e.Timestamp != "" && startedAt == "" {
			startedAt = e.Timestamp
		}
		if e.GitBranch != "" && gitBranch == "" {
			gitBranch = e.GitBranch
		}
		if startedAt != "" && gitBranch != "" {
			break
		}
	}

	subagentsDir := filepath.Join(projectDir, sessionID, "subagents")
	hasSubagents := dirExists(subagentsDir)

	return &SessionSummary{
		SessionID:        sessionID,
		Project:          shortName,
		Cwd:              cwd,
		GitBranch:        gitBranch,
		StartedAt:        startedAt,
		LastActivityAt:   mtime.UTC().Format(time.RFC3339),
		IsLive:           now.Sub(mtime) < liveWindow,
		FirstUserMessage: FirstUserMessage(parsed.Events, 140),
		TurnCount:        CountTurns(parsed.Events),
		SizeBytes:        sizeBytes,
		HasSubagents:     hasSubagents,
		lastActivityTime: mtime,
	}
}

// FindSessionPath returns the absolute JSONL path for a given session ID
// (or an empty string if not found).
func FindSessionPath(sessionID, dataDir string) string {
	root := ProjectsDir(dataDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		candidate := filepath.Join(root, e.Name(), sessionID+".jsonl")
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// FindProjectDir returns the per-session project-directory path (the
// directory holding both the .jsonl and the nested subagents/ dir).
func FindProjectDir(sessionID, dataDir string) string {
	root := ProjectsDir(dataDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		candidate := filepath.Join(root, e.Name(), sessionID+".jsonl")
		if fileExists(candidate) {
			return filepath.Join(root, e.Name())
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
