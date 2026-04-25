package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Coverage of the pure parts of the conversation-lifecycle code.
// SDK-touching paths are exercised in cli_test.go via fake-claude
// scripts; this file tests the local state-machinery.

func newConvAgent(t *testing.T, idle time.Duration, max int) *cliAgent {
	t.Helper()
	a := &cliAgent{
		cfg: Config{
			MaxConversations:        max,
			ConversationIdleTimeout: idle,
			DefaultBudget:           Budget{MaxTokens: 32768, MaxToolCalls: 20, Timeout: 2 * time.Minute},
		},
		convs: map[ConvID]*conversation{},
	}
	return a
}

// addFakeConv inserts a synthetic conversation directly into the map
// without going through StartConversation (which would still work
// without an SDK call but allocates a real UUID).  Tests that need
// deterministic convIDs use this.
func (a *cliAgent) addFakeConv(id string, lastActivityAt time.Time, sysPreview, userPreview string, turns int) *conversation {
	conv := &conversation{
		convID:           ConvID(id),
		startedAt:        lastActivityAt.Add(-time.Minute),
		lastActivityAt:   lastActivityAt,
		turnCount:        turns,
		systemPreview:    sysPreview,
		firstUserPreview: userPreview,
	}
	a.convsMu.Lock()
	a.convs[conv.convID] = conv
	a.convsMu.Unlock()
	return conv
}

func TestListConversations_SortedByLastActivityDesc(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	now := time.Now()
	a.addFakeConv("a", now.Add(-3*time.Minute), "", "first prompt A", 1)
	a.addFakeConv("b", now.Add(-1*time.Minute), "", "first prompt B", 2)
	a.addFakeConv("c", now.Add(-2*time.Minute), "", "first prompt C", 3)

	got := a.ListConversations()
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	wantIDs := []string{"b", "c", "a"}
	for i, want := range wantIDs {
		if string(got[i].ConvID) != want {
			t.Errorf("at index %d: convID=%q, want %q", i, got[i].ConvID, want)
		}
	}
}

func TestListConversations_OmitsEnded(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	now := time.Now()
	a.addFakeConv("alive", now, "", "alive", 1)
	dead := a.addFakeConv("ended", now.Add(time.Minute), "", "dead", 1)
	dead.ended = true

	got := a.ListConversations()
	if len(got) != 1 || got[0].ConvID != "alive" {
		t.Errorf("expected only one alive conv; got %+v", got)
	}
}

func TestListConversations_PreviewFallsBackToSystemPrompt(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.addFakeConv("c1", time.Now(), "you are an investigator", "", 0)
	got := a.ListConversations()
	if got[0].Preview != "you are an investigator" {
		t.Errorf("Preview=%q, want system_prompt fallback", got[0].Preview)
	}
}

func TestEndConversation_UnknownConvErrors(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	if _, err := a.EndConversation(context.Background(), "no-such", EndedByCaller); err != ErrUnknownConv {
		t.Errorf("err=%v, want ErrUnknownConv", err)
	}
}

func TestEndConversation_SuccessFreesSlot(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.addFakeConv("c", time.Now(), "", "x", 4)

	summary, err := a.EndConversation(context.Background(), "c", EndedByCaller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !summary.Ended || summary.AlreadyEnded {
		t.Errorf("summary wrong: %+v", summary)
	}
	if summary.TotalTurns != 4 || summary.EndedReason != EndedByCaller {
		t.Errorf("summary mismatched: %+v", summary)
	}
	// Subsequent ListConversations should omit it.
	if len(a.ListConversations()) != 0 {
		t.Errorf("ended conv still listed")
	}
}

func TestEndConversation_IdempotentReEnd(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	conv := a.addFakeConv("c", time.Now(), "", "x", 4)
	conv.ended = true
	conv.endedReason = EndedByIdleReap

	summary, err := a.EndConversation(context.Background(), "c", EndedByCaller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !summary.Ended || !summary.AlreadyEnded {
		t.Errorf("expected Ended && AlreadyEnded; got %+v", summary)
	}
	if summary.EndedReason != EndedByIdleReap {
		t.Errorf("endedReason=%v, want preserved (idle_timeout)", summary.EndedReason)
	}
}

func TestLookupAlive_DiscriminatesEndModes(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.addFakeConv("alive", time.Now(), "", "x", 0)
	r := a.addFakeConv("reaped", time.Now(), "", "y", 0)
	r.ended = true
	r.endedReason = EndedByIdleReap
	c := a.addFakeConv("caller", time.Now(), "", "z", 0)
	c.ended = true
	c.endedReason = EndedByCaller

	if _, err := a.lookupAlive("alive"); err != nil {
		t.Errorf("alive: err=%v", err)
	}
	if _, err := a.lookupAlive("reaped"); err != ErrConvReaped {
		t.Errorf("reaped: err=%v, want ErrConvReaped", err)
	}
	if _, err := a.lookupAlive("caller"); err != ErrUnknownConv {
		t.Errorf("caller-ended: err=%v, want ErrUnknownConv", err)
	}
	if _, err := a.lookupAlive("missing"); err != ErrUnknownConv {
		t.Errorf("missing: err=%v, want ErrUnknownConv", err)
	}
}

func TestAliveCountLocked_OnlyCountsNonEnded(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.addFakeConv("a", time.Now(), "", "x", 0)
	a.addFakeConv("b", time.Now(), "", "y", 0)
	c := a.addFakeConv("c", time.Now(), "", "z", 0)
	c.ended = true

	a.convsMu.Lock()
	defer a.convsMu.Unlock()
	if got := a.aliveCountLocked(); got != 2 {
		t.Errorf("aliveCountLocked=%d, want 2", got)
	}
}

func TestReapStaleConversations_MarksOldEntries(t *testing.T) {
	a := newConvAgent(t, 100*time.Millisecond, 0)
	now := time.Now()
	a.addFakeConv("old", now.Add(-time.Hour), "", "x", 1)
	a.addFakeConv("fresh", now, "", "y", 1)

	a.reapStaleConversations()

	if !a.convs["old"].ended {
		t.Errorf("old conversation should be reaped")
	}
	if a.convs["fresh"].ended {
		t.Errorf("fresh conversation must not be reaped")
	}
	if a.convs["old"].endedReason != EndedByIdleReap {
		t.Errorf("endedReason=%v, want idle_timeout", a.convs["old"].endedReason)
	}
}

func TestStartReaper_NoOpWhenIdleZero(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.startReaper()
	if a.reaperStop != nil || a.reaperDone != nil {
		t.Errorf("reaper should not start when ConversationIdleTimeout is zero")
	}
}

func TestStopReaper_IsIdempotent(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.stopReaper() // no-op when not started
	a.cfg.ConversationIdleTimeout = 100 * time.Millisecond
	a.startReaper()
	a.stopReaper()
	a.stopReaper() // double-call must be safe
}

func TestTruncateRunes_HappyAndEdges(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 3, "abc"},
		{"abc", 100, "abc"},
		{"αβγδ", 3, "αβγ"},
		{"abcdef", 0, ""},
	}
	for _, c := range cases {
		got := truncateRunes(c.in, c.n)
		if got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	preview := truncateRunes(strings.Repeat("a", 200), 140)
	if len([]rune(preview)) != 140 {
		t.Errorf("preview rune length=%d, want 140", len([]rune(preview)))
	}
}

func TestResolveProject_Defaults(t *testing.T) {
	a := &cliAgent{cfg: Config{FallbackProject: "/tmp"}}
	if got := a.resolveProject(""); got != "/tmp" {
		t.Errorf("empty project should resolve to FallbackProject; got %q", got)
	}
	if got := a.resolveProject("/no/such/path"); got != "" {
		t.Errorf("missing path should resolve to \"\"; got %q", got)
	}
	if got := a.resolveProject("/etc/hosts"); got != "" {
		t.Errorf("file path should resolve to \"\" (must be a directory); got %q", got)
	}
	dir := t.TempDir()
	if got := a.resolveProject(dir); got != dir {
		t.Errorf("temp dir should resolve as-is; got %q", got)
	}
}

func TestNewSessionUUID_Distinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := newSessionUUID()
		if seen[id] {
			t.Errorf("collision: %q", id)
		}
		seen[id] = true
	}
}

func TestConversation_LockSerializesTurns(t *testing.T) {
	conv := &conversation{convID: "c"}

	var wg sync.WaitGroup
	wg.Add(1)
	hold := make(chan struct{})
	go func() {
		defer wg.Done()
		conv.mu.Lock()
		<-hold
		conv.mu.Unlock()
	}()

	for {
		if !conv.mu.TryLock() {
			break
		}
		conv.mu.Unlock()
		time.Sleep(time.Millisecond)
	}

	contended := make(chan struct{})
	go func() {
		conv.mu.Lock()
		close(contended)
		conv.mu.Unlock()
	}()

	select {
	case <-contended:
		t.Errorf("contender acquired the lock while holder was active")
	case <-time.After(20 * time.Millisecond):
	}

	close(hold)
	wg.Wait()
	<-contended
}

func TestStartConversation_RespectsCap(t *testing.T) {
	a := newConvAgent(t, 0, 2)
	a.cfg.FallbackProject = t.TempDir()
	for i := 0; i < 2; i++ {
		if _, _, _, err := a.StartConversation(context.Background(), StartReq{}); err != nil {
			t.Fatalf("start #%d: %v", i, err)
		}
	}
	if _, _, _, err := a.StartConversation(context.Background(), StartReq{}); err != ErrConvCap {
		t.Errorf("third start: err=%v, want ErrConvCap", err)
	}
}

func TestStartConversation_RejectsInvalidProject(t *testing.T) {
	a := newConvAgent(t, 0, 0)
	a.cfg.FallbackProject = t.TempDir()
	_, _, _, err := a.StartConversation(context.Background(), StartReq{Project: "/no/such/path"})
	if err == nil {
		t.Errorf("expected invalid-project error")
	}
}
