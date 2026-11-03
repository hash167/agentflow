package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExtractVerdictPlain(t *testing.T) {
	v, err := ExtractVerdict(`{"summary":"ok","findings":[]}`)
	require.NoError(t, err)
	require.Equal(t, "ok", v.Summary)
	require.NotNil(t, v.Findings)
}

func TestExtractVerdictFencedWithProse(t *testing.T) {
	text := "Here is my review:\n```json\n{\"summary\":\"bug {braces} in string\",\"findings\":[{\"file\":\"b/a.go\",\"line\":3,\"severity\":\"ERROR\",\"message\":\"x\"}]}\n```\nThanks"
	v, err := ExtractVerdict(text)
	require.NoError(t, err)
	require.Equal(t, "bug {braces} in string", v.Summary)
	require.Equal(t, "a.go", v.Findings[0].File)
	require.Equal(t, "error", v.Findings[0].Severity)
}

func TestExtractVerdictSkipsNonVerdictObjects(t *testing.T) {
	v, err := ExtractVerdict(`{"note":1} then {"summary":"s","findings":null}`)
	require.NoError(t, err)
	require.Equal(t, "s", v.Summary)
	require.Empty(t, v.Findings)
}

func TestExtractVerdictRejects(t *testing.T) {
	for name, text := range map[string]string{
		"no json":       "I could not review this.",
		"bad severity":  `{"summary":"s","findings":[{"file":"a","severity":"critical","message":"m"}]}`,
		"empty summary": `{"summary":"  ","findings":[]}`,
		"no message":    `{"summary":"s","findings":[{"file":"a","severity":"info","message":""}]}`,
		"negative line": `{"summary":"s","findings":[{"file":"a","line":-1,"severity":"info","message":"m"}]}`,
		"truncated":     `{"summary":"s","findings":[`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ExtractVerdict(text)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidOutput))
		})
	}
}

func TestParseCLIOutput(t *testing.T) {
	text, err := ParseCLIOutput([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"{\"summary\":\"x\"}","session_id":"s"}` + "\n"))
	require.NoError(t, err)
	require.Equal(t, `{"summary":"x"}`, text)

	_, err = ParseCLIOutput([]byte(`{"type":"result","is_error":true,"result":"boom"}`))
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrInvalidOutput), "CLI error results are retryable")

	_, err = ParseCLIOutput([]byte("not json"))
	require.True(t, errors.Is(err, ErrInvalidOutput))
}

func TestMCPConfigReferencesKeyByName(t *testing.T) {
	t.Setenv(GatewayKeyEnv, "super-secret-value")
	raw := MCPConfig("http://gw/mcp", GatewayKeyEnv)
	require.NotContains(t, string(raw), "super-secret-value")
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Equal(t, "http://gw/mcp", cfg.MCPServers[MCPServerName].URL)
	require.Equal(t, "Bearer ${env:GATEWAY_REVIEWER_KEY}", cfg.MCPServers[MCPServerName].Headers["Authorization"])
}

// fakeAgent writes a shell script that records its argv and the workspace
// files, then prints the given CLI stdout.
func fakeAgent(t *testing.T, stdout string, exit int) (string, string) {
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	out := filepath.Join(dir, "stdout")
	require.NoError(t, os.WriteFile(out, []byte(stdout), 0o600))
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n" +
		"cat .cursor/mcp.json >> " + record + " 2>/dev/null\n" +
		"echo HOME=$HOME >> " + record + "\n" +
		"cat " + out + "\nexit " + string(rune('0'+exit)) + "\n"
	bin := filepath.Join(dir, "agent")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	return bin, record
}

func runWith(t *testing.T, bin string, input string) (int, string, string) {
	t.Setenv("CURSOR_API_KEY", "test-key")
	t.Setenv(GatewayKeyEnv, "gw-secret")
	cfg := Config{AgentBin: bin, Model: "grok-4.7", Timeout: 30 * time.Second, GatewayURL: "http://gw/mcp", GatewayKeyEnv: GatewayKeyEnv}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), cfg, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const sampleInput = `{"repo":"o/r","prNumber":1,"title":"t","diff":"+++ b/a.go\n@@ -0,0 +1 @@\n+x"}`

func TestRunSuccess(t *testing.T) {
	bin, record := fakeAgent(t, `{"type":"result","subtype":"success","is_error":false,"result":"`+"```json\\n{\\\"summary\\\":\\\"fine\\\",\\\"findings\\\":[]}\\n```"+`"}`, 0)
	code, stdout, stderr := runWith(t, bin, sampleInput)
	require.Equal(t, ExitOK, code, stderr)
	require.JSONEq(t, `{"summary":"fine","findings":[]}`, stdout)
	require.NotContains(t, stderr, "gw-secret")

	rec, err := os.ReadFile(record)
	require.NoError(t, err)
	for _, want := range []string{"--print", "--output-format\njson", "--model\ngrok-4.7", "--trust", "--disable-auto-update", "--approve-mcps", "--workspace", "${env:GATEWAY_REVIEWER_KEY}", "HOME="} {
		require.Contains(t, string(rec), want)
	}
	require.NotContains(t, string(rec), "gw-secret", "key must not be written to disk")
}

func TestRunInvalidVerdictExits3(t *testing.T) {
	bin, _ := fakeAgent(t, `{"type":"result","subtype":"success","is_error":false,"result":"Sorry, I cannot help."}`, 0)
	code, stdout, stderr := runWith(t, bin, sampleInput)
	require.Equal(t, ExitInvalidOutput, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "invalid reviewer output")
}

func TestRunAgentFailureExits1(t *testing.T) {
	bin, _ := fakeAgent(t, "", 1)
	code, _, _ := runWith(t, bin, sampleInput)
	require.Equal(t, ExitAgentFailed, code)
}

func TestRunBadInputExits2(t *testing.T) {
	bin, _ := fakeAgent(t, "", 0)
	code, _, _ := runWith(t, bin, "nope")
	require.Equal(t, ExitBadInput, code)
}

func TestRunUsesPerReviewModelFromStdin(t *testing.T) {
	bin, record := fakeAgent(t, `{"type":"result","subtype":"success","is_error":false,"result":"`+"```json\\n{\\\"summary\\\":\\\"fine\\\",\\\"findings\\\":[]}\\n```"+`"}`, 0)
	input := `{"repo":"o/r","prNumber":1,"title":"t","model":"claude-opus-5-5-high","diff":"+++ b/a.go\n@@ -0,0 +1 @@\n+x"}`
	code, _, stderr := runWith(t, bin, input)
	require.Equal(t, ExitOK, code, stderr)
	rec, err := os.ReadFile(record)
	require.NoError(t, err)
	require.Contains(t, string(rec), "--model\nclaude-opus-5-5-high")
	require.NotContains(t, string(rec), "grok-4.7")
}
