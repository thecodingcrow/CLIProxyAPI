package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Anthropic answers 400 "Claude Code X does not support this model" to
// claude-fable-5-2 from any reported version below this.
var claudeFable52MinVersion = [3]int{2, 1, 290}

var reportedClaudeVersionPattern = regexp.MustCompile(`(?:claude-cli/|cc_version=)(\d+)\.(\d+)\.(\d+)`)

func reportedClaudeVersionAtLeast(t *testing.T, field, value string, floor [3]int) {
	t.Helper()
	m := reportedClaudeVersionPattern.FindStringSubmatch(value)
	if m == nil {
		t.Fatalf("%s = %q, want a Claude Code version", field, value)
	}
	for i := 0; i < 3; i++ {
		part, _ := strconv.Atoi(m[i+1])
		if part != floor[i] {
			if part < floor[i] {
				t.Fatalf("%s = %q, want version >= %d.%d.%d", field, value, floor[0], floor[1], floor[2])
			}
			return
		}
	}
}

// Agent SDK sessions (entrypoint sdk-ts) are cloaked, so the API judges the
// model by the pinned baseline, never by the SDK's bundled CLI version.
func TestClaudeExecutor_AgentSDKFable52ReportsSupportedClaudeCodeVersion(t *testing.T) {
	var seenBody []byte
	var seenHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		seenHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "oauth-agent-sdk-fable-5-2",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-agent-sdk-fable",
			"base_url": server.URL,
		},
		Metadata: claudeOAuthTestMetadata(),
	}
	payload := []byte(`{"model":"claude-fable-5-2","system":"agent-sdk-system","messages":[{"role":"user","content":"reply ok"}],"metadata":{"user_id":"agent-sdk-user"}}`)
	_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-fable-5-2",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Headers: http.Header{
			"User-Agent":     {"claude-cli/2.1.296 (external, sdk-ts, agent-sdk/0.3.296)"},
			"X-App":          {"cli"},
			"Anthropic-Beta": {"claude-code-20250219"},
		},
	})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	reportedClaudeVersionAtLeast(t, "User-Agent", seenHeaders.Get("User-Agent"), claudeFable52MinVersion)
	reportedClaudeVersionAtLeast(t, "billing header", gjson.GetBytes(seenBody, "system.0.text").String(), claudeFable52MinVersion)
}
