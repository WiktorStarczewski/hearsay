package tailscale

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeTailscale creates a shell-script stub named `tailscale` in a
// dedicated temp dir and prepends that dir to PATH. Scripts can inspect
// $1/$2/... and emit controlled stdout/exit codes, letting us exercise
// DetectIPv4 / SelfDNSName deterministically without a live tailnet.
func writeFakeTailscale(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tailscale: %v", err)
	}
	// Prepend to PATH so LookPath finds our stub before any real binary.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDetectIPv4_ParsesValidOutput(t *testing.T) {
	writeFakeTailscale(t, "#!/bin/sh\necho 100.64.1.2\necho fe80::1234\n")
	got := DetectIPv4()
	if got != "100.64.1.2" {
		t.Errorf("DetectIPv4 = %q, want 100.64.1.2", got)
	}
}

func TestDetectIPv4_ReturnsEmptyOnMissingBinary(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	if got := DetectIPv4(); got != "" {
		t.Errorf("expected empty on missing binary, got %q", got)
	}
}

func TestDetectIPv4_ReturnsEmptyOnNonIPv4Output(t *testing.T) {
	writeFakeTailscale(t, "#!/bin/sh\necho garbage\necho fe80::1\n")
	if got := DetectIPv4(); got != "" {
		t.Errorf("expected empty when no ipv4 lines, got %q", got)
	}
}

func TestSelfDNSName_StripsTrailingDot(t *testing.T) {
	writeFakeTailscale(t, `#!/bin/sh
cat <<'EOF'
{"Self":{"DNSName":"ivan-mac.tail1234.ts.net.","HostName":"ivan-mac"}}
EOF
`)
	got := SelfDNSName()
	if got != "ivan-mac.tail1234.ts.net" {
		t.Errorf("SelfDNSName = %q", got)
	}
}

func TestSelfDNSName_FallsBackToHostName(t *testing.T) {
	writeFakeTailscale(t, `#!/bin/sh
cat <<'EOF'
{"Self":{"DNSName":"","HostName":"ivan-mac"}}
EOF
`)
	got := SelfDNSName()
	if got != "ivan-mac" {
		t.Errorf("SelfDNSName fallback = %q", got)
	}
}

func TestSelfDNSName_ReturnsEmptyOnJSONParseFailure(t *testing.T) {
	writeFakeTailscale(t, "#!/bin/sh\necho not-json\n")
	if got := SelfDNSName(); got != "" {
		t.Errorf("expected empty on bad json, got %q", got)
	}
}

func TestSelfDNSName_ReturnsEmptyOnBinaryMissing(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-"+t.Name())
	if got := SelfDNSName(); got != "" {
		t.Errorf("expected empty on missing binary, got %q", got)
	}
}

// Sanity check: when the fake tailscale exits non-zero, both functions
// treat that as "unavailable" rather than panicking.
func TestBothFunctions_SurviveCommandFailure(t *testing.T) {
	writeFakeTailscale(t, fmt.Sprintf("#!/bin/sh\n%s", "exit 1\n"))
	if got := DetectIPv4(); got != "" {
		t.Errorf("DetectIPv4 on failing command = %q", got)
	}
	if got := SelfDNSName(); got != "" {
		t.Errorf("SelfDNSName on failing command = %q", got)
	}
	// Regex sanity (not really exercised by the above, so touch it
	// directly to keep the coverage profile honest).
	if !ipv4Re.MatchString("1.2.3.4") {
		t.Errorf("ipv4Re should match '1.2.3.4'")
	}
	if ipv4Re.MatchString("not-an-ip") {
		t.Errorf("ipv4Re should reject non-IPv4")
	}
	_ = strings.TrimSpace // keep the strings import used even if tests reshuffle
}