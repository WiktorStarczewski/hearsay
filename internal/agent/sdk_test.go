package agent

import (
	"strings"
	"testing"
)

// TestBuildCustomToolUnion_OnlyAllowedToolsAdvertised is the
// first leg of the adversarial defense (verification step 7a in the
// plan).  The outgoing BetaAgentNewParams.Tools must contain exactly
// the three custom tools — no `agent_toolset_20260401`, no
// `mcp_toolset`, no extras.  Widening this list is a Tier-3 follow-up,
// not a Phase-2 knob.
func TestBuildCustomToolUnion_OnlyAllowedToolsAdvertised(t *testing.T) {
	tools := buildCustomToolUnion()
	if len(tools) != len(AllowedToolNames) {
		t.Fatalf("len(tools) = %d, want %d (one per AllowedToolNames entry)", len(tools), len(AllowedToolNames))
	}

	seenNames := map[string]bool{}
	for i, tu := range tools {
		// Every entry must use the OfCustom variant.  OfAgentToolset20260401
		// would route execution to an Anthropic-hosted sandbox, NOT
		// Ivan's filesystem; OfMCPToolset would require an inbound
		// public URL and undo Phase-1's tailnet-only privacy posture.
		if tu.OfAgentToolset20260401 != nil {
			t.Errorf("tools[%d] uses OfAgentToolset20260401; that bundle runs in Anthropic's sandbox, not on Ivan's box", i)
		}
		if tu.OfMCPToolset != nil {
			t.Errorf("tools[%d] uses OfMCPToolset; mcp_toolset would require an inbound public URL", i)
		}
		if tu.OfCustom == nil {
			t.Fatalf("tools[%d] has no Custom variant set", i)
		}
		seenNames[tu.OfCustom.Name] = true
	}

	for _, want := range AllowedToolNames {
		if !seenNames[want] {
			t.Errorf("missing required custom tool %q in outgoing Tools list", want)
		}
	}

	// Sanity: schemas are non-empty so the model knows how to call us.
	for _, tu := range tools {
		ct := tu.OfCustom
		if ct.Description == "" {
			t.Errorf("custom tool %q has empty description", ct.Name)
		}
		if ct.InputSchema.Properties == nil {
			t.Errorf("custom tool %q has nil input schema properties", ct.Name)
		}
	}
}

func TestCustomToolParam_ReadGlobGrepAreDistinct(t *testing.T) {
	read := customToolParam("read")
	glob := customToolParam("glob")
	grep := customToolParam("grep")
	if read == nil || glob == nil || grep == nil {
		t.Fatalf("expected all three custom-tool params")
	}
	for _, p := range []*struct {
		name string
		desc string
	}{
		{read.Name, read.Description},
		{glob.Name, glob.Description},
		{grep.Name, grep.Description},
	} {
		if p.desc == "" {
			t.Errorf("missing description for %s", p.name)
		}
	}
	if read.Name == glob.Name || glob.Name == grep.Name {
		t.Errorf("custom tool names must be distinct")
	}
}

func TestCustomToolParam_UnknownNameReturnsNil(t *testing.T) {
	if p := customToolParam("bash"); p != nil {
		t.Errorf("customToolParam(bash) should be nil; got %+v", p)
	}
}

// TestNew_RequiresAPIKey is a fast guard against accidentally
// constructing a useless agent at startup.
func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Errorf("New with empty APIKey should error")
	}
}

// TestNew_DefaultsModelAndCwd confirms construction-time fallbacks.
func TestNew_DefaultsModelAndCwd(t *testing.T) {
	ag, err := New(Config{APIKey: "sk-ant-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sa := ag.(*sdkAgent)
	if sa.cfg.Model == "" {
		t.Errorf("Model should default to a non-empty value")
	}
	if sa.cfg.FallbackProject == "" {
		t.Errorf("FallbackProject should default to os.Getwd()")
	}
}

// TestResolveProject_Defaults exercises the cwd cascade.
func TestResolveProject_Defaults(t *testing.T) {
	a := &sdkAgent{cfg: Config{FallbackProject: "/tmp"}}
	if got := a.resolveProject(""); got != "/tmp" {
		t.Errorf("empty project should resolve to FallbackProject; got %q", got)
	}
}

func TestResolveProject_RejectsMissing(t *testing.T) {
	a := &sdkAgent{cfg: Config{FallbackProject: "/tmp"}}
	if got := a.resolveProject("/nonexistent/please/no"); got != "" {
		t.Errorf("missing path should resolve to \"\"; got %q", got)
	}
}

func TestResolveProject_RejectsFile(t *testing.T) {
	a := &sdkAgent{cfg: Config{FallbackProject: "/tmp"}}
	// /etc/hosts exists on every macOS / Linux box and is a regular file.
	if got := a.resolveProject("/etc/hosts"); got != "" {
		t.Errorf("file path should resolve to \"\" (must be a directory); got %q", got)
	}
}

func TestResolveProject_AcceptsDir(t *testing.T) {
	a := &sdkAgent{cfg: Config{FallbackProject: "/tmp"}}
	if got := a.resolveProject(t.TempDir()); got == "" {
		t.Errorf("temp dir should resolve")
	}
}

// TestAllowedToolNames_IsReadOnly ensures we don't accidentally widen
// the read-only Phase-2 allowlist into "bash" / "edit" / "write" /
// dangerous mutating tools without a conscious intent.
func TestAllowedToolNames_IsReadOnly(t *testing.T) {
	allowed := strings.Join(AllowedToolNames, ",")
	for _, mutating := range []string{"bash", "edit", "write", "shell", "exec"} {
		if strings.Contains(allowed, mutating) {
			t.Errorf("AllowedToolNames must NOT include %q in Phase 2; widening is a Tier-3 follow-up", mutating)
		}
	}
}
