package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	projectpkg "github.com/Gentleman-Programming/engram/v3/internal/project"
)

// claudeEngramWriteAndSessionTools is the complete set of Engram MCP tools
// that mutate memory or session state. Claude's PreToolUse hook binds only
// these calls to its authoritative session identity; read-only MCP calls and
// every non-Engram server retain their existing contracts.
var claudeEngramWriteAndSessionTools = []string{
	"mem_save",
	"mem_update",
	"mem_review",
	"mem_delete",
	"mem_save_prompt",
	"mem_pin",
	"mem_unpin",
	"mem_session_summary",
	"mem_session_start",
	"mem_session_end",
	"mem_capture_passive",
	"mem_merge_projects",
	"mem_judge",
	"mem_compare",
}

var claudeEngramToolPrefixes = []string{
	"mcp__engram__",
	"mcp__plugin_engram_engram__",
}

var claudeHookOutput = func(response []byte) error {
	_, err := os.Stdout.Write(response)
	return err
}

func cmdHook(args []string) {
	if len(args) == 1 && args[0] == "claude-session-end" {
		input, err := io.ReadAll(os.Stdin)
		if err == nil {
			err = endClaudeHookInput(input)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Claude session closure skipped:", err)
			exitFunc(1)
		}
		return
	}
	if len(args) == 1 && args[0] == "claude-session-register" {
		input, err := io.ReadAll(os.Stdin)
		var response []byte
		if err == nil {
			response, err = registerClaudeHookInput(input)
		}
		if err == nil {
			err = claudeHookOutput(response)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Claude session registration failed:", err)
			exitFunc(1)
		}
		return
	}
	if len(args) == 1 && args[0] == "codex-user-prompt-submit" {
		cmdCodexUserPromptSubmit()
		return
	}
	if len(args) != 1 || (args[0] != "claude-pre-tool-use" && args[0] != "codex-pre-tool-use") {
		fmt.Fprintln(os.Stderr, "usage: engram hook claude-session-end|claude-session-register|claude-pre-tool-use|codex-pre-tool-use|codex-user-prompt-submit")
		exitFunc(1)
		return
	}

	input, err := io.ReadAll(os.Stdin)
	response := claudePreToolUseDeny("cannot read authoritative Claude hook input")
	if args[0] == "codex-pre-tool-use" {
		response = claudePreToolUseDeny("cannot read authoritative Codex hook input")
		if err == nil {
			response = guardCodexPreToolUse(input)
		}
	} else if err == nil {
		response = guardClaudePreToolUse(input)
	}
	if err := claudeHookOutput(response); err != nil {
		exitFunc(1)
		return
	}
}

// guardClaudePreToolUse confirms the host session before binding a mutating tool.
// Read-only and non-Engram calls never contact the server.
func guardClaudePreToolUse(input []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(input, &payload) != nil || payload == nil {
		return claudePreToolUseDeny("malformed authoritative Claude hook input")
	}
	tool, ok := claudeHookRequiredString(payload, "tool_name")
	if !ok {
		return claudePreToolUseDeny("authoritative Claude tool_name is required")
	}
	if !isClaudeEngramWriteOrSessionTool(tool) {
		return transformClaudePreToolUse(input)
	}
	for _, prefix := range claudeEngramToolPrefixes {
		if tool == prefix+"mem_session_end" {
			return claudePreToolUseDeny("Claude host lifecycle owns session closure; model mem_session_end is disabled")
		}
	}
	response, err := registerClaudeHookInput(input)
	if err != nil {
		return claudePreToolUseDeny("Claude host session registration could not be confirmed")
	}
	var acknowledged map[string]json.RawMessage
	_ = json.Unmarshal(response, &acknowledged)
	payload["session_id"] = acknowledged["id"]
	bound, _ := json.Marshal(payload)
	return transformClaudePreToolUse(bound)
}

// registerClaudeHookInput consumes only host metadata, never a model tool ID.
func registerClaudeHookInput(input []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(input, &payload) != nil {
		return nil, fmt.Errorf("malformed host input")
	}
	id, idOK := claudeHookRequiredString(payload, "session_id")
	cwd, cwdOK := claudeHookRequiredString(payload, "cwd")
	if !idOK || !cwdOK {
		return nil, fmt.Errorf("host session_id and cwd are required")
	}
	effective, err := registerClaudeSession(id, cwd)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"id": effective})
}

// endClaudeHookInput never registers: repeated host end cannot allocate a continuation.
func endClaudeHookInput(input []byte) error {
	var payload map[string]json.RawMessage
	if json.Unmarshal(input, &payload) != nil {
		return fmt.Errorf("malformed host input")
	}
	id, idOK := claudeHookRequiredString(payload, "session_id")
	cwd, cwdOK := claudeHookRequiredString(payload, "cwd")
	if !idOK || !cwdOK {
		return fmt.Errorf("host session_id and cwd are required")
	}
	base, client, err := claudeSessionTransport()
	if err != nil {
		return err
	}
	// Only host closure rejects redirects. Preserve the selected transport,
	// including Unix sockets, without altering registration or Codex policy.
	closeClient := *client
	closeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	authority, err := claudeEndJSON(ctx, &closeClient, http.MethodGet, base+"/project/current?cwd="+url.QueryEscape(cwd), nil)
	if err != nil {
		return fmt.Errorf("project resolution failed: %w", err)
	}
	authorityJSON, _ := json.Marshal(authority)
	project, ok := codexProjectAuthority(authorityJSON)
	if !ok {
		return fmt.Errorf("invalid project authority")
	}
	body, _ := json.Marshal(map[string]any{"effective_continuation": true, "project": project, "directory": projectpkg.RuntimeWorktreeDirectory(cwd), "ownership_mode": "project_owned"})
	result, err := claudeEndJSON(ctx, &closeClient, http.MethodPost, base+"/sessions/"+url.PathEscape(id)+"/end", body)
	if err != nil {
		return fmt.Errorf("session close failed: %w", err)
	}
	status, ok := claudeHookRequiredString(result, "status")
	if ok {
		switch status {
		case "completed":
			// The trusted server selects the effective continuation; it need
			// not match the host ID and must never be inferred from a suffix.
			if _, valid := claudeHookRequiredString(result, "id"); valid {
				return nil
			}
		case "no_active_session":
			if _, present := result["id"]; !present {
				return nil
			}
		}
	}
	return fmt.Errorf("invalid session close acknowledgement")
}

// claudeEndJSON is closure-local: require HTTP 200 and a complete JSON object.
// Read one byte beyond the cap to detect oversized bodies, including whitespace.
const claudeEndResponseMaxBytes = 64 << 10

func claudeEndJSON(ctx context.Context, client *http.Client, method, target string, body []byte) (map[string]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, claudeEndResponseMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > claudeEndResponseMaxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", claudeEndResponseMaxBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, fmt.Errorf("response must be one complete JSON object")
	}
	return object, nil
}

func claudeSessionTransport() (string, *http.Client, error) {
	base := strings.TrimSpace(os.Getenv("ENGRAM_URL"))
	client := &http.Client{}
	if base == "" {
		if socket := strings.TrimSpace(os.Getenv("ENGRAM_SOCKET")); socket != "" {
			base = "http://localhost"
			client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}}
		} else {
			port := strings.TrimSpace(os.Getenv("ENGRAM_PORT"))
			n, err := strconv.Atoi(port)
			if port == "" {
				n = 7437
			} else if err != nil || n < 1 || n > 65535 {
				return "", nil, fmt.Errorf("invalid ENGRAM_PORT")
			}
			base = fmt.Sprintf("http://127.0.0.1:%d", n)
		}
	}
	return strings.TrimRight(base, "/"), client, nil
}

func registerClaudeSession(id, cwd string) (string, error) {
	base, client, err := claudeSessionTransport()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var authority json.RawMessage
	if !codexJSON(ctx, client, http.MethodGet, base+"/project/current?cwd="+url.QueryEscape(cwd), nil, &authority) {
		return "", fmt.Errorf("project resolution failed")
	}
	project, ok := codexProjectAuthority(authority)
	if !ok {
		return "", fmt.Errorf("invalid project authority")
	}
	body, _ := json.Marshal(map[string]any{"id": id, "project": project, "directory": cwd, "ownership_mode": "project_owned", "resume": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sessions", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var ack struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		ResumedFrom string `json:"resumed_from"`
	}
	decoder := json.NewDecoder(resp.Body)
	if resp.StatusCode != http.StatusCreated || decoder.Decode(&ack) != nil || decoder.Decode(&struct{}{}) != io.EOF || strings.TrimSpace(ack.ID) == "" || ack.Status != "created" || (ack.ID != id && ack.ResumedFrom != id) {
		return "", fmt.Errorf("invalid registration acknowledgement")
	}
	return ack.ID, nil
}

func confirmHookSession(id, cwd string, projectOwned bool) bool {
	base := strings.TrimSpace(os.Getenv("ENGRAM_URL"))
	client := &http.Client{}
	if base == "" {
		if socket := strings.TrimSpace(os.Getenv("ENGRAM_SOCKET")); socket != "" {
			base = "http://localhost"
			client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}}
		} else {
			port := strings.TrimSpace(os.Getenv("ENGRAM_PORT"))
			n, err := strconv.Atoi(port)
			if port == "" {
				n = 7437
			} else if err != nil || n < 1 || n > 65535 {
				return false
			}
			base = fmt.Sprintf("http://127.0.0.1:%d", n)
		}
	}
	base = strings.TrimRight(base, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var authority json.RawMessage
	if !codexJSON(ctx, client, http.MethodGet, base+"/project/current?cwd="+url.QueryEscape(cwd), nil, &authority) {
		return false
	}
	project, ok := codexProjectAuthority(authority)
	if !ok {
		return false
	}
	registration := map[string]string{"id": id, "project": project, "directory": cwd}
	if projectOwned {
		registration["ownership_mode"] = "project_owned"
	}
	body, _ := json.Marshal(registration)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sessions", strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return false
	}
	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	return json.NewDecoder(resp.Body).Decode(&result) == nil && result.ID == id && result.Status == "created"
}

// transformClaudePreToolUse consumes Claude Code's authoritative PreToolUse
// input. It never grants a permission decision: successful calls use only
// updatedInput to replace the tool's untrusted model-provided session reference.
// Invalid hook input is denied, rather than allowing a write whose session cannot be bound.
func transformClaudePreToolUse(input []byte) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(input, &payload); err != nil {
		return claudePreToolUseDeny("malformed authoritative Claude hook input")
	}

	sessionID, ok := claudeHookRequiredString(payload, "session_id")
	if !ok {
		return claudePreToolUseDeny("authoritative Claude session_id is required")
	}
	toolName, ok := claudeHookRequiredString(payload, "tool_name")
	if !ok {
		return claudePreToolUseDeny("authoritative Claude tool_name is required")
	}

	toolInputRaw, ok := payload["tool_input"]
	if !ok {
		return claudePreToolUseDeny("authoritative Claude tool_input is required")
	}
	var toolInput map[string]json.RawMessage
	if err := json.Unmarshal(toolInputRaw, &toolInput); err != nil || toolInput == nil {
		return claudePreToolUseDeny("authoritative Claude tool_input must be an object")
	}

	if !isClaudeEngramWriteOrSessionTool(toolName) {
		return []byte("{}")
	}

	boundSessionID, err := json.Marshal(sessionID)
	if err != nil {
		return claudePreToolUseDeny("cannot bind authoritative Claude session_id")
	}
	toolInput[claudeAuthoritativeSessionField(toolName)] = boundSessionID
	updatedInput, err := json.Marshal(toolInput)
	if err != nil {
		return claudePreToolUseDeny("cannot encode bound Claude tool input")
	}

	return claudePreToolUseResponse(updatedInput)
}

// guardCodexPreToolUse confirms only mutating Engram calls against the host session.
func guardCodexPreToolUse(input []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(input, &payload) != nil || payload == nil {
		return claudePreToolUseDeny("malformed authoritative Codex hook input")
	}
	tool, ok := claudeHookRequiredString(payload, "tool_name")
	if !ok {
		return claudePreToolUseDeny("authoritative Codex tool_name is required")
	}
	if !isClaudeEngramWriteOrSessionTool(tool) {
		return transformCodexPreToolUse(input)
	}
	id, idOK := claudeHookRequiredString(payload, "session_id")
	cwd, cwdOK := claudeHookRequiredString(payload, "cwd")
	if !idOK || !cwdOK || !confirmHookSession(id, cwd, false) {
		return claudePreToolUseDeny("Codex host session registration could not be confirmed")
	}
	return transformCodexPreToolUse(input)
}

// Codex requires an explicit allow alongside updatedInput for MCP argument rewrites.
// Reuse Claude's classification and binder; a denial remains a denial.
func transformCodexPreToolUse(input []byte) []byte {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(input, &payload); err != nil || payload == nil {
		return claudePreToolUseDeny("malformed authoritative Codex hook input")
	}
	toolName, ok := claudeHookRequiredString(payload, "tool_name")
	if !ok {
		return claudePreToolUseDeny("authoritative Codex tool_name is required")
	}
	if !isClaudeEngramWriteOrSessionTool(toolName) {
		return []byte("{}")
	}
	response := transformClaudePreToolUse(input)
	var envelope struct {
		HookSpecificOutput map[string]json.RawMessage `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return claudePreToolUseDeny("cannot encode Codex hook response")
	}
	if _, bound := envelope.HookSpecificOutput["updatedInput"]; !bound {
		return response
	}
	envelope.HookSpecificOutput["permissionDecision"] = json.RawMessage(`"allow"`)
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return claudePreToolUseDeny("cannot encode Codex hook response")
	}
	return encoded
}

func claudeHookRequiredString(payload map[string]json.RawMessage, field string) (string, bool) {
	raw, ok := payload[field]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func claudeAuthoritativeSessionField(toolName string) string {
	if strings.HasSuffix(toolName, "__mem_session_start") || strings.HasSuffix(toolName, "__mem_session_end") {
		return "id"
	}
	return "session_id"
}

func isClaudeEngramWriteOrSessionTool(name string) bool {
	for _, prefix := range claudeEngramToolPrefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		tool := strings.TrimPrefix(name, prefix)
		for _, writeTool := range claudeEngramWriteAndSessionTools {
			if tool == writeTool {
				return true
			}
		}
	}
	return false
}

func claudePreToolUseResponse(updatedInput json.RawMessage) []byte {
	response := struct {
		HookSpecificOutput struct {
			HookEventName string          `json:"hookEventName"`
			UpdatedInput  json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}{}
	response.HookSpecificOutput.HookEventName = "PreToolUse"
	response.HookSpecificOutput.UpdatedInput = updatedInput
	encoded, _ := json.Marshal(response)
	return encoded
}

func claudePreToolUseDeny(reason string) []byte {
	response := struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}{}
	response.HookSpecificOutput.HookEventName = "PreToolUse"
	response.HookSpecificOutput.PermissionDecision = "deny"
	response.HookSpecificOutput.PermissionDecisionReason = reason
	encoded, _ := json.Marshal(response)
	return encoded
}
