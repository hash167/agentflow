// A deterministic stand-in for an LLM review agent, so the control plane can
// be exercised end-to-end without an API key. It reads {"repo","prNumber",
// "title","diff"} JSON on stdin, scans added lines in the unified diff for
// common smells, and prints a {"summary","findings"} JSON verdict on stdout.
//
// In production, point review.reviewerCommand at a script that invokes the
// headless Cursor CLI instead (see README).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type input struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"prNumber"`
	Title    string `json:"title"`
	Diff     string `json:"diff"`
}

type finding struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type verdict struct {
	Summary  string    `json:"summary"`
	Findings []finding `json:"findings"`
}

var (
	hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	checks = []struct {
		pattern  *regexp.Regexp
		severity string
		message  string
	}{
		{regexp.MustCompile(`\b(TODO|FIXME)\b`), "info", "Leftover TODO/FIXME marker"},
		{regexp.MustCompile(`\b(fmt\.Println|console\.log|print\()`), "warning", "Possible leftover debug output"},
		{regexp.MustCompile(`\bpanic\(`), "warning", "panic() in changed code; prefer returning an error"},
		{regexp.MustCompile(`(?i)(password|secret|api_?key)\s*:?=\s*["'][^"']+["']`), "error", "Possible hardcoded credential"},
	}
)

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mock-reviewer: cannot read stdin:", err)
		os.Exit(1)
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(os.Stderr, "mock-reviewer: invalid input JSON:", err)
		os.Exit(1)
	}

	findings := []finding{}
	files := map[string]bool{}
	currentFile := ""
	lineNumber := 0

	for _, line := range strings.Split(in.Diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			currentFile = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if currentFile != "/dev/null" {
				files[currentFile] = true
			}
		case hunkRe.MatchString(line):
			start, _ := strconv.Atoi(hunkRe.FindStringSubmatch(line)[1])
			lineNumber = start - 1
		case strings.HasPrefix(line, "+"):
			lineNumber++
			added := line[1:]
			for _, check := range checks {
				if check.pattern.MatchString(added) {
					findings = append(findings, finding{
						File:     currentFile,
						Line:     lineNumber,
						Severity: check.severity,
						Message:  check.message,
					})
				}
			}
		case strings.HasPrefix(line, "-"):
			// removed line: does not advance the new-file line counter
		default:
			lineNumber++
		}
	}

	summary := fmt.Sprintf("Mock review of %s#%d: %d changed file(s), %d finding(s).",
		in.Repo, in.PRNumber, len(files), len(findings))
	_ = json.NewEncoder(os.Stdout).Encode(verdict{Summary: summary, Findings: findings})
}
