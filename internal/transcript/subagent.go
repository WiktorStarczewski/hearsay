package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SubagentMeta is the sidecar .meta.json for a subagent session.
type SubagentMeta struct {
	AgentType   string `json:"agentType,omitempty"`
	Description string `json:"description,omitempty"`
}

// SubagentResolution is the result of finding a subagent session on disk.
type SubagentResolution struct {
	AgentID   string
	Meta      *SubagentMeta
	JSONLPath string
}

// ResolveSubagent locates the JSONL for a subagent spawned from the given
// parent session. The Agent tool_use id in the parent session's JSONL is
// the value we match against; Claude Code files subagent JSONLs under
//   <projectDir>/<sessionId>/subagents/<agentId>.jsonl
// or a prefixed `agent-<agentId>.jsonl`. We try both conventions and
// fall back to a substring scan for resilience.
func ResolveSubagent(sessionID, agentUUID, dataDir string) *SubagentResolution {
	projectDir := FindProjectDir(sessionID, dataDir)
	if projectDir == "" {
		return nil
	}
	subagentsDir := filepath.Join(projectDir, sessionID, "subagents")
	if !dirExists(subagentsDir) {
		return nil
	}

	candidates := []string{
		filepath.Join(subagentsDir, agentUUID+".jsonl"),
		filepath.Join(subagentsDir, "agent-"+agentUUID+".jsonl"),
	}
	for _, c := range candidates {
		if fileExists(c) {
			return buildResolution(c, agentUUID)
		}
	}

	// Fallback substring match — catches the "agent-<short-hex>.jsonl"
	// naming variant observed in the wild.
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if strings.Contains(name, agentUUID) {
			return buildResolution(filepath.Join(subagentsDir, name), strings.TrimSuffix(name, ".jsonl"))
		}
	}
	return nil
}

func buildResolution(jsonlPath, agentID string) *SubagentResolution {
	metaPath := strings.TrimSuffix(jsonlPath, ".jsonl") + ".meta.json"
	var meta *SubagentMeta
	if fileExists(metaPath) {
		if raw, err := os.ReadFile(metaPath); err == nil {
			var m SubagentMeta
			if err := json.Unmarshal(raw, &m); err == nil {
				meta = &m
			}
		}
	}
	return &SubagentResolution{AgentID: agentID, Meta: meta, JSONLPath: jsonlPath}
}

// sidecarPathRegex matches the absolute path a tool_result block embeds
// when Claude Code stores the result out-of-line. Example match:
//   /Users/.../projects/-Users-x-y/<sessionId>/tool-results/b1nml93nh.txt
var sidecarPathRegex = regexp.MustCompile(`/tool-results/[A-Za-z0-9_-]+\.txt`)

// ToolResultLocationKind discriminates sidecar-file vs inline content.
type ToolResultLocationKind int

const (
	ToolResultInline ToolResultLocationKind = iota
	ToolResultSidecar
)

// ToolResultLocation is the output of LocateToolResult. Callers use this
// to decide between reading a sidecar .txt file from disk or returning
// the inline content directly.
type ToolResultLocation struct {
	Kind        ToolResultLocationKind
	SidecarPath string
	InlineText  string
}

// LocateToolResult walks a session's JSONL to find the tool_result block
// for the given toolUseId, then inspects its content text to determine
// whether the actual payload is inline or in a sidecar file. See
// implementation flag #1 in the plan for the rationale: sidecar filenames
// are NOT the tool_use.id — the absolute path is embedded in content text.
func LocateToolResult(sessionJSONLPath, toolUseID string) *ToolResultLocation {
	raw, err := os.ReadFile(sessionJSONLPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev struct {
			Message *Message `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Message == nil {
			continue
		}
		// Content is either a string or a []block; tool_result lives in
		// the array variant.
		var blocks []map[string]any
		if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			t, _ := b["type"].(string)
			if t != "tool_result" {
				continue
			}
			if id, _ := b["tool_use_id"].(string); id != toolUseID {
				continue
			}
			text := extractText(b["content"])
			if text == "" {
				return nil
			}
			if m := sidecarPathRegex.FindString(text); m != "" {
				return &ToolResultLocation{Kind: ToolResultSidecar, SidecarPath: m}
			}
			return &ToolResultLocation{Kind: ToolResultInline, InlineText: text}
		}
	}
	return nil
}

func extractText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, raw := range blocks {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := b["type"].(string); t == "text" {
			if txt, _ := b["text"].(string); txt != "" {
				parts = append(parts, txt)
			}
		}
	}
	return strings.Join(parts, "\n")
}
