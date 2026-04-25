package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// conversation is the per-conv state hearsay tracks.  After PR C's
// subprocess pivot, the canonical record of every conversation lives
// on disk as a Claude Code session JSONL at
// `<dataDir>/projects/<encoded-cwd>/<convID>.jsonl`; this struct is
// metadata only.
type conversation struct {
	mu sync.Mutex // serializes turns within this conversation

	convID         ConvID
	started        bool // true after the first SendMessage; toggles --session-id → --resume
	startedAt      time.Time
	lastActivityAt time.Time
	turnCount      int
	perTurnBudget  Budget
	project        string
	systemPrompt   string // full system prompt; first SendMessage passes it via --system-prompt

	systemPreview    string
	firstUserPreview string

	ended       bool
	endedReason EndReason
	endedAt     time.Time
}

// StartConversation allocates a UUID + slot, records the system
// prompt, and returns.  No subprocess is spawned here — Claude Code
// creates the on-disk JSONL lazily on the first --print call against
// the convID (variant 2 of the argv contract).
func (a *cliAgent) StartConversation(ctx context.Context, req StartReq) (ConvID, time.Time, Budget, error) {
	project := a.resolveProject(req.Project)
	if project == "" && req.Project != "" {
		return "", time.Time{}, Budget{}, fmt.Errorf("%w: %q", errInvalidProject, req.Project)
	}

	a.convsMu.Lock()
	if a.cfg.MaxConversations > 0 && a.aliveCountLocked() >= a.cfg.MaxConversations {
		a.convsMu.Unlock()
		return "", time.Time{}, Budget{}, ErrConvCap
	}
	a.convsMu.Unlock()

	convID := ConvID(newSessionUUID())
	now := time.Now()
	effective := req.Budget.Resolve(a.cfg.DefaultBudget)
	conv := &conversation{
		convID:         convID,
		startedAt:      now,
		lastActivityAt: now,
		perTurnBudget:  effective,
		project:        project,
		systemPrompt:   req.SystemPrompt,
		systemPreview:  truncateRunes(req.SystemPrompt, 140),
	}

	a.convsMu.Lock()
	if a.convs == nil {
		a.convs = make(map[ConvID]*conversation)
	}
	a.convs[convID] = conv
	a.convsMu.Unlock()

	return convID, now, effective, nil
}

// SendMessage drives one more turn of an existing conversation.
// First-turn calls invoke `claude --print --session-id <conv.convID>
// [--system-prompt <conv.systemPrompt>]`; subsequent turns invoke
// `claude --print --resume <conv.convID>`.
func (a *cliAgent) SendMessage(ctx context.Context, convID ConvID, prompt string, budget Budget) (Transcript, error) {
	conv, err := a.lookupAlive(convID)
	if err != nil {
		return Transcript{}, err
	}

	conv.mu.Lock()
	defer conv.mu.Unlock()

	// Re-check liveness inside the lock so a mid-turn reap+restart
	// race produces ErrConvReaped, not a confusing error from claude.
	a.convsMu.Lock()
	live, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok || live.ended {
		return Transcript{}, ErrConvReaped
	}

	turnBudget := budget.Resolve(conv.perTurnBudget)
	if turnBudget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, turnBudget.Timeout)
		defer cancel()
	}

	conv.turnCount++
	if conv.firstUserPreview == "" {
		conv.firstUserPreview = truncateRunes(prompt, 140)
	}
	turnIndex := conv.turnCount

	tx, invokes, _ := a.runClaude(ctx, runReq{
		conv:       conv,
		prompt:     prompt,
		budget:     turnBudget,
		convStarted: conv.started,
	})
	conv.lastActivityAt = time.Now()
	if !conv.started && tx.StopReason != StopReasonShutdown && tx.StopReason != StopReasonTimeout {
		// Flip iff the subprocess actually ran; if we shut down or
		// timed out before the subprocess could create the JSONL,
		// the next attempt should re-include --system-prompt.
		conv.started = true
	}

	if a.cfg.Auditor != nil {
		_ = a.cfg.Auditor.Log(AuditEntry{
			Timestamp:     time.Now().UTC(),
			PeerName:      a.cfg.PeerName,
			ConvID:        string(convID),
			TurnIndex:     turnIndex,
			PromptBytes:   len(prompt),
			ResponseBytes: len(tx.Markdown),
			ToolCalls:     invokes,
			ElapsedMs:     tx.ElapsedMs,
			StopReason:    tx.StopReason,
			ErrorSummary:  tx.ErrorSummary,
		})
	}
	tx.TurnCount = turnIndex
	return tx, nil
}

// ListConversations returns the metadata view of every alive
// conversation, sorted by lastActivityAt desc.
func (a *cliAgent) ListConversations() []ConvMeta {
	a.convsMu.Lock()
	defer a.convsMu.Unlock()

	out := make([]ConvMeta, 0, len(a.convs))
	for _, c := range a.convs {
		if c.ended {
			continue
		}
		preview := c.firstUserPreview
		if preview == "" {
			preview = c.systemPreview
		}
		out = append(out, ConvMeta{
			ConvID:         c.convID,
			StartedAt:      c.startedAt,
			LastActivityAt: c.lastActivityAt,
			TurnCount:      c.turnCount,
			Preview:        preview,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivityAt.After(out[j].LastActivityAt)
	})
	return out
}

// EndConversation marks the conv slot ended.  No upstream call —
// Claude Code's session JSONL stays on disk so Phase-1 read_session
// can still surface it.  Idempotent: re-end returns AlreadyEnded.
func (a *cliAgent) EndConversation(ctx context.Context, convID ConvID, reason EndReason) (EndSummary, error) {
	a.convsMu.Lock()
	conv, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok {
		return EndSummary{}, ErrUnknownConv
	}

	conv.mu.Lock()
	if conv.ended {
		summary := EndSummary{
			Ended:        true,
			AlreadyEnded: true,
			TotalTurns:   conv.turnCount,
			EndedReason:  conv.endedReason,
		}
		conv.mu.Unlock()
		return summary, nil
	}
	conv.ended = true
	conv.endedReason = reason
	conv.endedAt = time.Now()
	totalTurns := conv.turnCount
	conv.mu.Unlock()

	return EndSummary{
		Ended:        true,
		AlreadyEnded: false,
		TotalTurns:   totalTurns,
		EndedReason:  reason,
	}, nil
}

// lookupAlive returns the conversation iff it exists and hasn't been
// ended.  Returns ErrUnknownConv on miss / caller-ended; ErrConvReaped
// if the idle reaper terminated it.
func (a *cliAgent) lookupAlive(convID ConvID) (*conversation, error) {
	a.convsMu.Lock()
	conv, ok := a.convs[convID]
	a.convsMu.Unlock()
	if !ok {
		return nil, ErrUnknownConv
	}
	conv.mu.Lock()
	ended := conv.ended
	endedReason := conv.endedReason
	conv.mu.Unlock()
	if ended {
		if endedReason == EndedByIdleReap {
			return nil, ErrConvReaped
		}
		return nil, ErrUnknownConv
	}
	return conv, nil
}

// aliveCountLocked counts non-ended conversations.  Caller MUST hold
// a.convsMu.
func (a *cliAgent) aliveCountLocked() int {
	count := 0
	for _, c := range a.convs {
		if !c.ended {
			count++
		}
	}
	return count
}

// startReaper launches the background idle-reaper.
func (a *cliAgent) startReaper() {
	if a.cfg.ConversationIdleTimeout <= 0 {
		return
	}
	a.reaperStop = make(chan struct{})
	a.reaperDone = make(chan struct{})
	go a.reapLoop()
}

func (a *cliAgent) reapLoop() {
	defer close(a.reaperDone)

	interval := a.cfg.ConversationIdleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-a.reaperStop:
			return
		case <-ticker.C:
			a.reapStaleConversations()
		}
	}
}

// reapStaleConversations marks any conversation whose lastActivityAt
// is older than --conversation-idle-timeout as ended.  No upstream
// delete — the JSONL stays on disk for Phase-1 read_session.
func (a *cliAgent) reapStaleConversations() {
	cutoff := time.Now().Add(-a.cfg.ConversationIdleTimeout)
	a.convsMu.Lock()
	defer a.convsMu.Unlock()
	for _, conv := range a.convs {
		conv.mu.Lock()
		if !conv.ended && conv.lastActivityAt.Before(cutoff) {
			conv.ended = true
			conv.endedReason = EndedByIdleReap
			conv.endedAt = time.Now()
		}
		conv.mu.Unlock()
	}
}

// stopReaper signals the reaper goroutine to exit and waits for it.
func (a *cliAgent) stopReaper() {
	if a.reaperStop == nil {
		return
	}
	select {
	case <-a.reaperStop:
	default:
		close(a.reaperStop)
	}
	if a.reaperDone != nil {
		<-a.reaperDone
	}
}

// truncateRunes returns the first n runes of s as a new string.
// UTF-8 safe — iterates by rune so a multi-byte codepoint at the
// boundary doesn't yield invalid UTF-8 in the output.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// resolveProject implements the OneShotRequest/StartReq Project rules:
//
//  1. If req.Project is non-empty AND points at an existing dir, use it.
//  2. If req.Project is non-empty but invalid, return "" (caller surfaces invalid_project).
//  3. If req.Project is empty, use FallbackProject (set to os.Getwd() at construction).
func (a *cliAgent) resolveProject(p string) string {
	if p == "" {
		return a.cfg.FallbackProject
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return ""
	}
	return p
}

// errInvalidProject is the typed sentinel for invalid req.Project.
var errInvalidProject = errors.New("invalid project")

// newSessionUUID returns a random UUIDv4 string.  Used as the convID
// for new conversations and as the --session-id argv argument.
//
// We don't import a UUID library — this is a 16-byte random buffer
// formatted into the standard 8-4-4-4-12 hex layout with the
// version/variant bits set per RFC 4122.
func newSessionUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is essentially fatal on every
		// platform we ship to; falling back to a fixed UUID would
		// silently corrupt the conv map.  Panic so the operator
		// notices.
		panic(fmt.Sprintf("agent: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
