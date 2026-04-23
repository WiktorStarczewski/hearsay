// Package claudemd manages the optional discoverability block that
// hearsay writes into ~/.claude/CLAUDE.md. Two disjoint blocks are
// supported — "consumer" (read my teammates' sessions) and "peer"
// (share your session ID with the reader) — each with its own HTML-
// comment marker pair so they coexist without collision.
package claudemd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Role string

const (
	RoleConsumer Role = "consumer"
	RolePeer     Role = "peer"
)

// Action describes what install/uninstall actually did to the file.
type Action string

const (
	ActionCreated  Action = "created"
	ActionReplaced Action = "replaced"
	ActionAppended Action = "appended"
	ActionRemoved  Action = "removed"
	ActionNoop     Action = "noop"
)

// Result is the return value from install/uninstall — enough context for
// main.go to print a useful status line.
type Result struct {
	Action Action
	Path   string
}

type markers struct {
	start string
	end   string
}

func markersFor(role Role) markers {
	if role == RolePeer {
		return markers{
			start: "<!-- hearsay:peer-auto-start -->",
			end:   "<!-- hearsay:peer-auto-end -->",
		}
	}
	return markers{
		start: "<!-- hearsay:consumer-auto-start -->",
		end:   "<!-- hearsay:consumer-auto-end -->",
	}
}

const consumerBlockBody = `## hearsay — reading my teammates' Claude Code sessions

Several of my teammates run ` + "`hearsay`" + ` MCP servers that expose their Claude Code session transcripts. Each server is labelled with the teammate's first name in my MCP config. When I mention a teammate by name, route to the correspondingly-named tools (` + "`mcp__<name>__*`" + `). For "what's <name> doing right now?", prefer ` + "`get_current_session`" + ` — it carries its own ambiguity contract in the tool description.

Don't relay questions through me — read the source transcript.`

const peerBlockBody = `## hearsay — sharing session IDs with Wiktor

When Wiktor asks about a test run you did, or when you report a bug to him, include your current session ID in the message. Get it with: Bash command ` + "`ls -1t ~/.claude/projects/*/*.jsonl | head -1 | xargs basename -s .jsonl`" + ` (walks every project dir, returns the most recent session ID overall). Wiktor uses the ` + "`hearsay`" + ` MCP server to read transcripts directly, and ` + "`get_current_session`" + ` usually disambiguates — but pasting an ID eliminates guesswork when you have multiple Claudes open.`

func blockFor(role Role) string {
	m := markersFor(role)
	body := consumerBlockBody
	if role == RolePeer {
		body = peerBlockBody
	}
	return m.start + "\n" + body + "\n" + m.end
}

// DefaultPath returns ~/.claude/CLAUDE.md, regardless of platform.
// Claude Code uses this path on both macOS and Linux.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "CLAUDE.md")
}

// Print returns the block text (with trailing newline) for stdout piping.
func Print(role Role) string {
	return blockFor(role) + "\n"
}

// Install writes or updates the block at the given CLAUDE.md path. The
// behavior matches the four cases documented in the plan:
//  1. missing file → create with the block only
//  2. both markers present → replace in place
//  3. only one marker present → refuse (caller exits non-zero)
//  4. no markers → append
func Install(role Role, path string) (*Result, error) {
	if path == "" {
		path = DefaultPath()
	}
	block := blockFor(role)
	m := markersFor(role)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(path, []byte(block+"\n"), 0o644); err != nil {
				return nil, err
			}
			return &Result{Action: ActionCreated, Path: path}, nil
		}
		return nil, err
	}

	text := string(raw)
	hasStart := strings.Contains(text, m.start)
	hasEnd := strings.Contains(text, m.end)

	if hasStart && hasEnd {
		startIdx := strings.Index(text, m.start)
		endIdx := strings.Index(text, m.end) + len(m.end)
		if endIdx <= startIdx {
			return nil, fmt.Errorf("end marker appears before start marker in %s; manual repair needed", path)
		}
		next := text[:startIdx] + block + text[endIdx:]
		if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
			return nil, err
		}
		return &Result{Action: ActionReplaced, Path: path}, nil
	}

	if hasStart || hasEnd {
		orphan := m.start
		if hasEnd {
			orphan = m.end
		}
		return nil, fmt.Errorf(
			"found orphaned marker %q in %s without its partner — refusing to auto-repair. "+
				"Remove the orphan manually, then re-run 'hearsay claude-md install'.",
			orphan, path,
		)
	}

	sep := "\n"
	if !strings.HasSuffix(text, "\n") {
		sep = "\n\n"
	} else if !strings.HasSuffix(text, "\n\n") {
		sep = "\n"
	}
	next := text + sep + block + "\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return nil, err
	}
	return &Result{Action: ActionAppended, Path: path}, nil
}

// Uninstall removes the block. Noop when the file or markers are absent.
// Refuses on an orphaned-marker state (same policy as Install).
func Uninstall(role Role, path string) (*Result, error) {
	if path == "" {
		path = DefaultPath()
	}
	m := markersFor(role)

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Result{Action: ActionNoop, Path: path}, nil
		}
		return nil, err
	}

	text := string(raw)
	hasStart := strings.Contains(text, m.start)
	hasEnd := strings.Contains(text, m.end)

	if !hasStart && !hasEnd {
		return &Result{Action: ActionNoop, Path: path}, nil
	}
	if hasStart != hasEnd {
		orphan := m.start
		if hasEnd {
			orphan = m.end
		}
		return nil, fmt.Errorf(
			"found orphaned marker %q in %s without its partner — refusing to auto-repair",
			orphan, path,
		)
	}

	startIdx := strings.Index(text, m.start)
	endIdx := strings.Index(text, m.end) + len(m.end)
	next := text[:startIdx] + text[endIdx:]
	// Collapse runs of 3+ newlines into 2 so the file doesn't develop blank-line holes.
	for strings.Contains(next, "\n\n\n") {
		next = strings.ReplaceAll(next, "\n\n\n", "\n\n")
	}
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return nil, err
	}
	return &Result{Action: ActionRemoved, Path: path}, nil
}
