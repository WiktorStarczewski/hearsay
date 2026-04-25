package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// customToolHandler is the per-name dispatch target.  Inputs come from
// agent.custom_tool_use events as JSON-decoded maps; the handler
// returns the body string the agent will see in its tool_result.
//
// All three handlers are bounded:
//   - read caps at maxReadBytes
//   - glob caps the result list at maxGlobResults entries
//   - grep caps the result list at maxGrepMatches entries
type customToolHandler func(input map[string]any, root string) (string, error)

const (
	maxReadBytes    = 64 * 1024
	maxGlobResults  = 200
	maxGrepMatches  = 200
	maxGrepFileSize = 2 * 1024 * 1024
)

// customToolHandlers maps tool name → handler.  Used both by the SDK
// agent (to dispatch incoming custom_tool_use) and by the adversarial
// test (to confirm only these three names are advertised).
var customToolHandlers = map[string]customToolHandler{
	"read": readToolHandler,
	"glob": globToolHandler,
	"grep": grepToolHandler,
}

// resolveUnderRoot returns an absolute path that's guaranteed to live
// under root.  Symlink-resolved.  Errors if the resolved path escapes
// root via .. or symlink.
func resolveUnderRoot(root, p string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		// Don't fail just because the root has dangling symlinks
		// somewhere; we only need the root itself to resolve.
		rootResolved = rootAbs
	}

	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(rootResolved, p)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Path may not exist yet; fall back to the cleaned absolute
		// (still detects ../ escapes).
		resolved = filepath.Clean(abs)
	}
	rel, err := filepath.Rel(rootResolved, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project root: %s", p)
	}
	return resolved, nil
}

// readToolHandler implements the `read` custom tool.
// Schema: { file_path: string }
func readToolHandler(input map[string]any, root string) (string, error) {
	pathArg, ok := input["file_path"].(string)
	if !ok || pathArg == "" {
		return "", errors.New("read: missing file_path")
	}
	resolved, err := resolveUnderRoot(root, pathArg)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", pathArg, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read: %s is a directory", pathArg)
	}

	f, err := os.Open(resolved)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", pathArg, err)
	}
	defer f.Close()

	// Read at most maxReadBytes+1 so we can detect truncation without
	// loading the whole file.
	limited := io.LimitReader(f, int64(maxReadBytes)+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", pathArg, err)
	}
	body := string(raw)
	truncated := len(raw) > maxReadBytes
	if truncated {
		body = body[:maxReadBytes]
	}

	// Header includes the visible-to-agent metadata so it can decide
	// whether to ask for a different range / a smaller scope.
	header := fmt.Sprintf("[file=%s, bytes=%d, truncated=%t]\n\n", pathArg, len(body), truncated)
	return header + body, nil
}

// globToolHandler implements the `glob` custom tool.
// Schema: { pattern: string }
//
// Pattern is a doublestar-like ** glob, evaluated relative to root.
// Returns up to maxGlobResults paths, one per line, sorted.
func globToolHandler(input map[string]any, root string) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", errors.New("glob: missing pattern")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}

	results, err := walkGlob(rootAbs, pattern, maxGlobResults+1)
	if err != nil {
		return "", err
	}

	truncated := len(results) > maxGlobResults
	if truncated {
		results = results[:maxGlobResults]
	}

	header := fmt.Sprintf("[pattern=%s, matches=%d, truncated=%t]\n", pattern, len(results), truncated)
	return header + strings.Join(results, "\n") + "\n", nil
}

// walkGlob walks root, returning paths matching the pattern.  Stops as
// soon as cap matches accumulate (one over maxGlobResults so the
// caller can detect truncation).  Pattern uses filepath.Match
// semantics on each path component, with `**` collapsing
// any-number-of-directories.
func walkGlob(root, pattern string, cap int) ([]string, error) {
	patternHasDoublestar := strings.Contains(pattern, "**")
	var matches []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees
		}
		// Skip hidden dirs (.git, etc.)
		if d.IsDir() {
			name := d.Name()
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if matchGlob(pattern, rel, patternHasDoublestar) {
			matches = append(matches, rel)
			if len(matches) >= cap {
				return fs.SkipAll
			}
		}
		return nil
	})
	return matches, err
}

// matchGlob is filepath.Match plus `**` collapsing arbitrary path
// segments.  Implements just enough for the common "src/**/*.go"
// idiom; not a full ohshitglob clone.
func matchGlob(pattern, name string, hasDoublestar bool) bool {
	if !hasDoublestar {
		// Match against the path AND the basename so that "*.go"
		// works without forcing the user to write "**/*.go".
		ok, _ := filepath.Match(pattern, name)
		if ok {
			return true
		}
		ok, _ = filepath.Match(pattern, filepath.Base(name))
		return ok
	}

	// Split on `**`; each segment must match in order, with arbitrary
	// directory content between them.
	parts := strings.Split(pattern, "**")
	rest := name
	for i, part := range parts {
		part = strings.Trim(part, "/")
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(rest, part) && !strings.HasPrefix(rest, "/"+part) {
				// Fall back to component matching for patterns
				// like `**/*.go` where part is the trailing
				// extension match.
				if i == len(parts)-1 {
					ok, _ := filepath.Match(part, filepath.Base(name))
					if ok {
						return true
					}
				}
				return false
			}
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, "/"), part)
			continue
		}
		if i == len(parts)-1 {
			// Tail segment may be a per-component pattern
			ok, _ := filepath.Match(part, filepath.Base(name))
			if ok {
				return true
			}
			return strings.HasSuffix(rest, "/"+part) || strings.HasSuffix(rest, part)
		}
		idx := strings.Index(rest, "/"+part+"/")
		if idx < 0 {
			return false
		}
		rest = rest[idx+1+len(part):]
	}
	return true
}

// grepToolHandler implements the `grep` custom tool.
// Schema: { pattern: string, path?: string, max_results?: int }
func grepToolHandler(input map[string]any, root string) (string, error) {
	patternStr, ok := input["pattern"].(string)
	if !ok || patternStr == "" {
		return "", errors.New("grep: missing pattern")
	}
	scope := root
	if p, ok := input["path"].(string); ok && p != "" {
		resolved, err := resolveUnderRoot(root, p)
		if err != nil {
			return "", err
		}
		scope = resolved
	}
	cap := maxGrepMatches
	if v, ok := input["max_results"].(float64); ok && v > 0 && int(v) < cap {
		cap = int(v)
	}

	re, err := regexp.Compile(patternStr)
	if err != nil {
		return "", fmt.Errorf("grep: invalid regex: %w", err)
	}

	var lines []string
	count := 0
	truncated := false
	walkErr := filepath.WalkDir(scope, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != scope && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileSize {
			return nil
		}
		matches := grepFile(path, re, cap-count)
		for _, m := range matches {
			rel, _ := filepath.Rel(root, path)
			lines = append(lines, fmt.Sprintf("%s:%d: %s", rel, m.line, m.text))
			count++
			if count >= cap {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	header := fmt.Sprintf("[pattern=%q, matches=%d, truncated=%t]\n", patternStr, count, truncated)
	return header + strings.Join(lines, "\n") + "\n", nil
}

type grepMatch struct {
	line int
	text string
}

func grepFile(path string, re *regexp.Regexp, remaining int) []grepMatch {
	if remaining <= 0 {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if !isLikelyText(body) {
		return nil
	}
	var out []grepMatch
	for i, line := range strings.Split(string(body), "\n") {
		if re.MatchString(line) {
			// Cap individual line length so a giant minified line
			// doesn't blow up the response.
			if len(line) > 240 {
				line = line[:240] + "…"
			}
			out = append(out, grepMatch{line: i + 1, text: line})
			if len(out) >= remaining {
				break
			}
		}
	}
	return out
}

// isLikelyText is a cheap heuristic: NUL bytes in the first 8KB ⇒
// binary.  Avoids dumping random bytes from .so / .png / .pyc files.
func isLikelyText(body []byte) bool {
	n := len(body)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if body[i] == 0 {
			return false
		}
	}
	return true
}

