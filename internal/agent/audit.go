package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// AuditEntry is one line of agent.log.  Sizes only — no prompt /
// response / tool-arg content, no hashes (per the conservative-privacy
// posture in the plan).  An opt-in --agent-debug-log flag could carry
// raw content, but that's a future addition.
type AuditEntry struct {
	Timestamp     time.Time         `json:"timestamp"`
	PeerName      string            `json:"peer"`
	ConvID        string            `json:"convId"` // "oneshot" for ask_peer_claude
	TurnIndex     int               `json:"turnIndex"`
	PromptBytes   int               `json:"promptBytes"`
	ResponseBytes int               `json:"responseBytes"`
	ToolCalls     []AuditToolInvoke `json:"toolCalls,omitempty"`
	ElapsedMs     int64             `json:"elapsedMs"`
	StopReason    StopReason        `json:"stopReason"`
	ErrorSummary  ErrorSummary      `json:"errorSummary,omitempty"`
}

// AuditToolInvoke records a single tool call without its arguments —
// just the name and the byte size of the JSON-encoded args.
type AuditToolInvoke struct {
	Name     string `json:"name"`
	ArgBytes int    `json:"argBytes"`
}

// Auditor is a line-atomic JSONL writer guarded by a mutex.
// Concurrent agent calls produce non-interleaved appends.
type Auditor struct {
	mu sync.Mutex
	f  *os.File
}

// NewAuditor opens (or creates) the platform-appropriate agent.log,
// MkdirAll'ing the parent directory if needed.  Returns a no-op
// Auditor if path is empty (e.g. tests that don't care).
func NewAuditor(path string) (*Auditor, error) {
	if path == "" {
		return &Auditor{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &Auditor{f: f}, nil
}

// Log appends one JSON-serialized AuditEntry as a single line.
// Concurrent callers serialize via a mutex; each Write is line-atomic
// so partial-line interleaving is impossible even without the mutex,
// but the mutex avoids the rare case of a Write breaking up across
// page boundaries on some filesystems.
func (a *Auditor) Log(entry AuditEntry) error {
	if a == nil || a.f == nil {
		return nil
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.f.Write(raw)
	return err
}

// Close releases the underlying file.  Safe to call on a nil-file
// Auditor (returned for empty-path construction).
func (a *Auditor) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

// DefaultAuditPath returns the platform-appropriate agent.log path.
// On macOS, ~/Library/Logs/hearsay/agent.log.  On other systems,
// $XDG_STATE_HOME/hearsay/agent.log (default ~/.local/state/hearsay/...
// per the XDG Base Directory spec).
func DefaultAuditPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "hearsay", "agent.log")
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "hearsay", "agent.log")
	}
	return filepath.Join(home, ".local", "state", "hearsay", "agent.log")
}
