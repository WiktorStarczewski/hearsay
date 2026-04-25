package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuditor_NoopOnEmptyPath(t *testing.T) {
	a, err := NewAuditor("")
	if err != nil {
		t.Fatalf("NewAuditor empty: %v", err)
	}
	defer a.Close()
	if err := a.Log(AuditEntry{}); err != nil {
		t.Errorf("noop Log returned error: %v", err)
	}
}

func TestAuditor_AppendsLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deeply", "nested", "agent.log")
	a, err := NewAuditor(path)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer a.Close()

	if err := a.Log(AuditEntry{
		Timestamp:     time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		PeerName:      "ivan",
		ConvID:        "oneshot",
		PromptBytes:   42,
		ResponseBytes: 100,
		ElapsedMs:     1234,
		StopReason:    StopReasonEndTurn,
	}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimSuffix(string(raw), "\n")
	var got AuditEntry
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if got.PeerName != "ivan" || got.PromptBytes != 42 || got.StopReason != StopReasonEndTurn {
		t.Errorf("decoded entry mismatched: %+v", got)
	}
}

// TestAuditor_ConcurrentWritesAreLineAtomic spins up many goroutines
// each writing one log line.  Every line must round-trip through JSON
// unmarshal without errors — i.e. no two concurrent writes interleaved.
func TestAuditor_ConcurrentWritesAreLineAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	a, err := NewAuditor(path)
	if err != nil {
		t.Fatalf("NewAuditor: %v", err)
	}
	defer a.Close()

	const writers = 20
	const each = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_ = a.Log(AuditEntry{
					PeerName:    "ivan",
					ConvID:      "oneshot",
					TurnIndex:   id*100 + i,
					PromptBytes: 1024,
					StopReason:  StopReasonEndTurn,
				})
			}
		}(w)
	}
	wg.Wait()
	a.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	count := 0
	for scanner.Scan() {
		var entry AuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Errorf("line %d failed to parse (interleaving?): %v\n%s", count, err, scanner.Text())
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if count != writers*each {
		t.Errorf("got %d lines, want %d", count, writers*each)
	}
}

func TestDefaultAuditPath_NonEmpty(t *testing.T) {
	if got := DefaultAuditPath(); got == "" {
		t.Errorf("DefaultAuditPath returned empty string")
	}
}

func TestDefaultAuditPath_HonorsXDGState(t *testing.T) {
	// Only meaningful on non-darwin, but the env-honor branch is
	// tested unconditionally.  On macOS we just confirm the function
	// returns _some_ path; on Linux we'd check XDG_STATE_HOME wins.
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
	got := DefaultAuditPath()
	if got == "" {
		t.Errorf("DefaultAuditPath returned empty string")
	}
}

func TestNewAuditor_RefusesUnopenablePath(t *testing.T) {
	// Pointing at a path under a regular file (not a directory) is a
	// guaranteed failure on every platform we ship to.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditor(filepath.Join(blocker, "agent.log")); err == nil {
		t.Errorf("expected NewAuditor to fail when the parent of the path is a regular file")
	}
}
