package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/v3/internal/mcp"
	projectpkg "github.com/Gentleman-Programming/engram/v3/internal/project"
	"github.com/Gentleman-Programming/engram/v3/internal/server"
	"github.com/Gentleman-Programming/engram/v3/internal/store"
)

func claudeHookStdin(t *testing.T, input string, closed bool) *os.File {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdin pipe: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	if _, err := writer.Write([]byte(input)); err != nil {
		t.Fatalf("write Claude hook input: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}
	if closed {
		if err := reader.Close(); err != nil {
			t.Fatalf("close stdin reader: %v", err)
		}
	}
	return reader
}

// The test executable provides a deterministic process boundary for the shell;
// it runs the existing Go dispatch against the fixture server, never installed Engram.
func TestClaudeLifecycleRegistrationProcess(t *testing.T) {
	if os.Getenv("ENGRAM_TEST_CLAUDE_REGISTER_PROCESS") != "1" {
		return
	}
	cmdHook([]string{"claude-session-register"})
	os.Exit(0)
}

func TestClaudeEndedRegistrationCannotPersistBoundWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the Claude SessionStart bash hook")
	}
	for _, binary := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("requires %s: %v", binary, err)
		}
	}
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const host = "ended-claude-host"
	if err := db.CreateSession(host, "project-a", root); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(host, "finished"); err != nil {
		t.Fatal(err)
	}
	var registrationStatus int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/project/current":
			_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
		case r.URL.Path == "/sessions" && r.Method == http.MethodPost:
			var req struct {
				ID            string `json:"id"`
				Project       string `json:"project"`
				Directory     string `json:"directory"`
				OwnershipMode string `json:"ownership_mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("registration body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.ID != host || req.Project != "project-a" || req.OwnershipMode != "project_owned" {
				t.Errorf("registration = %+v", req)
			}
			err := db.StartSessionWithOwnershipMode(req.ID, req.Project, req.Directory, req.OwnershipMode)
			if !errors.Is(err, store.ErrSessionAlreadyEnded) {
				t.Errorf("registration error = %v, want already ended", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			registrationStatus = http.StatusConflict
			w.WriteHeader(registrationStatus)
			_, _ = w.Write([]byte(`{"code":"session_already_ended"}`))
		case r.URL.Path == "/context":
			_, _ = w.Write([]byte(`{"context":""}`))
		default:
			t.Errorf("unexpected hook request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	stubDir := filepath.Join(root, "bin")
	if err := os.Mkdir(stubDir, 0700); err != nil {
		t.Fatal(err)
	}
	// The shell consumes the Go helper failure, not a direct registration POST.
	// BASH_ENV intercepts every Engram invocation even under Windows Bash PATH.
	bashEnv := filepath.Join(stubDir, "engram-mock.sh")
	stub := `engram() {
  if [ "$*" = 'hook claude-session-register' ]; then
    input=$(cat)
    printf '%s' "$input" > "$ENGRAM_TEST_REGISTER_INPUT"
  fi
  return 1
}
`
	if err := os.WriteFile(bashEnv, []byte(stub), 0600); err != nil {
		t.Fatal(err)
	}
	registerInput := filepath.Join(root, "register-input")
	input, _ := json.Marshal(map[string]string{"session_id": host, "cwd": root})
	cmd := exec.Command("bash", filepath.Join("..", "..", "plugin", "claude-code", "scripts", "session-start.sh"))
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Env = append(os.Environ(), "BASH_ENV="+filepath.ToSlash(bashEnv), "ENGRAM_TEST_REGISTER_INPUT="+filepath.ToSlash(registerInput), "HOME="+root, "CLAUDE_CONFIG_DIR="+filepath.Join(root, "claude"), "ENGRAM_DATA_DIR="+filepath.Join(root, "data"), "ENGRAM_URL="+server.URL, "ENGRAM_SOCKET=", "ENGRAM_PROJECT=", "ENGRAM_PORT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SessionStart: %v: %s", err, out)
	}
	if registrationStatus != 0 || !strings.Contains(string(out), "registration unavailable") {
		t.Fatalf("shell bypassed shared registration failure: status %d, output %s", registrationStatus, out)
	}
	registeredInput, err := os.ReadFile(registerInput)
	if err != nil || string(registeredInput) != string(input) {
		t.Fatalf("native metadata not forwarded to helper: %q, %v", registeredInput, err)
	}
	request, _ := json.Marshal(map[string]any{"session_id": host, "cwd": root, "tool_name": "mcp__engram__mem_save", "tool_input": map[string]any{"title": "ended host write", "content": "must not persist", "session_id": "foreign-model-session", "project": "project-a"}})
	var hook struct {
		HookSpecificOutput struct {
			PermissionDecision string         `json:"permissionDecision"`
			UpdatedInput       map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	t.Setenv("ENGRAM_URL", server.URL)
	if err := json.Unmarshal(guardClaudePreToolUse(request), &hook); err != nil {
		t.Fatal(err)
	}
	if hook.HookSpecificOutput.PermissionDecision != "deny" || hook.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("ended host must be denied without bound input: %+v", hook)
	}
	// Claude does not dispatch denied tool calls to MCP.

	observations, err := db.AllObservations("project-a", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := db.SessionObservations(host, 100)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := db.GetSession(host)
	if err != nil {
		t.Fatal(err)
	}
	if ended.EndedAt == nil {
		t.Fatal("409 registration unexpectedly reopened ended session")
	}
	if _, err := db.GetSession("foreign-model-session"); err == nil {
		t.Fatal("foreign model session was created")
	}
	if len(observations) != 0 || len(bound) != 0 {
		t.Fatalf("ended host write persisted %d observations after 409", len(observations))
	}

	// Direct/manual MCP calls bypass the Claude hook; this is not an agent-path write.
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "mem_save", "arguments": map[string]any{"title": "manual write", "content": "direct MCP control", "session_id": host, "project": "project-a"}}})
	response := mcp.NewServerWithConfig(db, mcp.MCPConfig{DefaultProject: "project-a"}, nil).HandleMessage(context.Background(), call)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"isError":true`) || !strings.Contains(string(encoded), "manual write") {
		t.Fatalf("direct/manual MCP write result: %s", encoded)
	}
	observations, err = db.AllObservations("project-a", "", 100)
	if err != nil || len(observations) != 1 {
		t.Fatalf("direct/manual MCP persistence: count=%d, err=%v, response=%s", len(observations), err, encoded)
	}
}

func TestCodexEndedSessionStart409DeniesWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes Codex SessionStart bash hook")
	}
	for _, binary := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("requires %s: %v", binary, err)
		}
	}
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession("host", "project-a", root); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("host", "finished"); err != nil {
		t.Fatal(err)
	}
	production := server.New(db, 0).Handler()
	registrationStatus := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
			return
		}
		if r.URL.Path == "/context" {
			_, _ = w.Write([]byte(`{"context":""}`))
			return
		}
		if r.URL.Path != "/sessions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		capture := httptest.NewRecorder()
		production.ServeHTTP(capture, r)
		registrationStatus = capture.Code
		w.WriteHeader(capture.Code)
		_, _ = w.Write(capture.Body.Bytes())
	}))
	defer endpoint.Close()
	input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": root})
	command := exec.Command("bash", filepath.Join("..", "..", "plugin", "codex", "scripts", "session-start.sh"))
	command.Stdin = strings.NewReader(string(input))
	command.Env = append(os.Environ(), "HOME="+root, "ENGRAM_DATA_DIR="+filepath.Join(root, "data"), "ENGRAM_URL="+endpoint.URL, "ENGRAM_SOCKET=", "ENGRAM_PROJECT=", "ENGRAM_PORT=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex SessionStart: %v: %s", err, output)
	}
	if registrationStatus != 409 || strings.Contains(string(output), `"session_id":"host"`) || strings.Contains(string(output), "Registered runtime session") {
		t.Fatalf("409 handoff: status=%d output=%s", registrationStatus, output)
	}
	t.Setenv("ENGRAM_URL", endpoint.URL)
	response := guardCodexPreToolUse([]byte(`{"session_id":"host","cwd":"` + filepath.ToSlash(root) + `","tool_name":"mcp__engram__mem_save","tool_input":{"session_id":"model","project":"project-a","title":"blocked"}}`))
	var result struct {
		HookSpecificOutput struct {
			PermissionDecision string          `json:"permissionDecision"`
			UpdatedInput       json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(response, &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" || result.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("response=%s err=%v", response, err)
	}
	observations, err := db.AllObservations("project-a", "", 100)
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
	ended, err := db.GetSession("host")
	if err != nil || ended.EndedAt == nil {
		t.Fatalf("ended=%+v err=%v", ended, err)
	}
}

func TestCodexCallConfirmsSharedHostBeforeBinding(t *testing.T) {
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSessionWithOwnershipMode("host", "project-a", root, store.SessionOwnershipShared); err != nil {
		t.Fatal(err)
	}
	production := server.New(db, 0).Handler()
	registrations := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			if r.URL.Query().Get("cwd") != filepath.ToSlash(root) {
				t.Errorf("cwd = %q", r.URL.Query().Get("cwd"))
			}
			_, _ = w.Write([]byte(`{"project":"project-b","project_source":"config"}`))
			return
		}
		if r.URL.Path != "/sessions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		var registration struct {
			ID        string `json:"id"`
			Project   string `json:"project"`
			Ownership string `json:"ownership_mode"`
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(body, &registration); err != nil {
			t.Error(err)
		}
		if registration.ID != "host" || registration.Project != "project-b" || registration.Ownership != "" {
			t.Errorf("registration = %+v", registration)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		capture := httptest.NewRecorder()
		production.ServeHTTP(capture, r)
		var confirmation struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(capture.Body.Bytes(), &confirmation); err != nil {
			t.Error(err)
		}
		if capture.Code != http.StatusCreated || confirmation.ID != "host" || confirmation.Status != "created" {
			t.Errorf("registration response: %d %s", capture.Code, capture.Body.String())
		}
		registrations++
		w.WriteHeader(capture.Code)
		_, _ = w.Write(capture.Body.Bytes())
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	input := []byte(`{"session_id":"host","cwd":"` + filepath.ToSlash(root) + `","tool_name":"mcp__engram__mem_save","tool_input":{"session_id":"model","project":"project-b","title":"retained","content":"cross-project observation"}}`)
	var result struct {
		HookSpecificOutput struct {
			PermissionDecision string         `json:"permissionDecision"`
			UpdatedInput       map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(guardCodexPreToolUse(input), &result); err != nil {
		t.Fatal(err)
	}
	if registrations != 1 || result.HookSpecificOutput.PermissionDecision != "allow" || result.HookSpecificOutput.UpdatedInput["session_id"] != "host" || result.HookSpecificOutput.UpdatedInput["project"] != "project-b" {
		t.Fatalf("registrations=%d result=%+v", registrations, result)
	}
	call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "mem_save", "arguments": result.HookSpecificOutput.UpdatedInput}})
	mcpResult := mcp.NewServerWithConfig(db, mcp.MCPConfig{DefaultProject: "project-a"}, nil).HandleMessage(context.Background(), call)
	encoded, err := json.Marshal(mcpResult)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"isError":true`) {
		t.Fatalf("bound MCP write failed: %s", encoded)
	}
	observations, err := db.AllObservations("project-b", "", 100)
	if err != nil || len(observations) != 1 {
		t.Fatalf("project B observations=%d err=%v result=%s", len(observations), err, encoded)
	}
	bound, err := db.SessionObservations("host", 100)
	if err != nil || len(bound) != 1 || bound[0].ID != observations[0].ID {
		t.Fatalf("host observations=%v err=%v", bound, err)
	}
	owner, err := db.GetSession("host")
	if err != nil || owner.Project != "project-a" || owner.OwnershipMode != store.SessionOwnershipShared || owner.EndedAt != nil {
		t.Fatalf("shared owner=%+v err=%v", owner, err)
	}
	if _, err := db.GetSession("model"); err == nil {
		t.Fatal("foreign model session created")
	}
}

func TestCodexInvalidExplicitPortDenies(t *testing.T) {
	for _, port := range []string{"invalid", "0", "65536"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("ENGRAM_URL", "")
			t.Setenv("ENGRAM_SOCKET", "")
			t.Setenv("ENGRAM_PORT", port)
			response := guardCodexPreToolUse([]byte(`{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`))
			var result struct {
				HookSpecificOutput struct {
					PermissionDecision string          `json:"permissionDecision"`
					UpdatedInput       json.RawMessage `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(response, &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" || result.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("response=%s err=%v", response, err)
			}
		})
	}
}

func TestCodexUnconfirmedCallsDenyWithoutUpdatedInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"ended", 409, `{"code":"session_already_ended"}`},
		{"unavailable", 503, `{}`},
		{"wrong id", 201, `{"id":"other","status":"created"}`},
		{"wrong status", 201, `{"id":"host","status":"failed"}`},
		{"malformed", 201, `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/project/current" {
					_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer endpoint.Close()
			t.Setenv("ENGRAM_URL", endpoint.URL)
			response := guardCodexPreToolUse([]byte(`{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`))
			var result struct {
				HookSpecificOutput struct {
					PermissionDecision string          `json:"permissionDecision"`
					UpdatedInput       json.RawMessage `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(response, &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" || result.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("response=%s err=%v", response, err)
			}
		})
	}
}

func TestCodexGuardRejectsMissingHostIdentityWithoutNetwork(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unconfirmed call contacted server: %s %s", r.Method, r.URL)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"missing cwd", `{"session_id":"host","tool_name":"mcp__engram__mem_save","tool_input":{}}`},
		{"blank cwd", `{"session_id":"host","cwd":"  ","tool_name":"mcp__engram__mem_save","tool_input":{}}`},
		{"missing session", `{"cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`},
		{"blank session", `{"session_id":"  ","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result struct {
				HookSpecificOutput struct {
					PermissionDecision string          `json:"permissionDecision"`
					UpdatedInput       json.RawMessage `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			response := guardCodexPreToolUse([]byte(tc.input))
			if err := json.Unmarshal(response, &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" || result.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("want deny without updated input: %s, err=%v", response, err)
			}
		})
	}
}

func TestCodexGuardReadOnlyAndNonEngramSkipNetwork(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("read-only call contacted server: %s %s", r.Method, r.URL)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)

	for _, tool := range []string{"mcp__engram__mem_search", "mcp__plugin_engram_engram__mem_search", "Bash"} {
		t.Run(tool, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"tool_name": tool})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(guardCodexPreToolUse(input)); got != "{}" {
				t.Fatalf("want untouched read-only input, got %s", got)
			}
		})
	}
}

func TestClaudeInvalidExplicitPortDeniesWithoutDefaultServer(t *testing.T) {
	for _, port := range []string{"invalid", "0", "65536"} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("ENGRAM_URL", "")
			t.Setenv("ENGRAM_SOCKET", "")
			t.Setenv("ENGRAM_PORT", port)
			response := guardClaudePreToolUse([]byte(`{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`))
			var result struct {
				HookSpecificOutput struct {
					PermissionDecision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(response, &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("explicit invalid port must deny: %s, %v", response, err)
			}
		})
	}
}

func TestClaudeResumeBinding(t *testing.T) {
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.StartSessionWithOwnershipMode("host", "project-a", root, store.SessionOwnershipProjectOwned); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("host", "finished"); err != nil {
		t.Fatal(err)
	}
	srv := server.New(db, 0)
	authority := "project-a"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_ = json.NewEncoder(w).Encode(map[string]string{"project": authority, "project_source": "config"})
			return
		}
		srv.Handler().ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	for _, tool := range []string{"mem_save", "mem_session_start", "mem_save"} {
		input, _ := json.Marshal(map[string]any{"session_id": "host", "cwd": root, "tool_name": "mcp__engram__" + tool, "tool_input": map[string]string{"id": "model", "session_id": "model"}})
		var output struct {
			HookSpecificOutput struct {
				UpdatedInput map[string]string `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(guardClaudePreToolUse(input), &output); err != nil {
			t.Fatal(err)
		}
		field := "session_id"
		if tool == "mem_session_start" {
			field = "id"
		}
		if output.HookSpecificOutput.UpdatedInput[field] != "host:resume:2" {
			t.Fatalf("effective binding: %+v", output)
		}
		if tool == "mem_save" {
			if _, err := db.AddObservation(store.AddObservationParams{SessionID: output.HookSpecificOutput.UpdatedInput[field], Project: "project-a", Type: "note", Title: "resumed", Content: "bound"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	original, err := db.GetSession("host")
	if err != nil || original.EndedAt == nil {
		t.Fatalf("root reopened: %+v %v", original, err)
	}
	var writes int
	if err := db.DB().QueryRow(`SELECT count(*) FROM observations WHERE session_id = 'host:resume:2'`).Scan(&writes); err != nil || writes != 1 {
		t.Fatalf("attribution: %d %v", writes, err)
	}
	input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": root})
	authority = "project-b"
	if response, err := registerClaudeHookInput(input); err == nil || response != nil {
		t.Fatalf("foreign root allowed: %s %v", response, err)
	}
	authority = "project-a"
	if _, err := db.DB().Exec(`UPDATE sessions SET project = 'foreign' WHERE id = 'host:resume:2'`); err != nil {
		t.Fatal(err)
	}
	if response, err := registerClaudeHookInput(input); err == nil || response != nil {
		t.Fatalf("foreign continuation allowed: %s %v", response, err)
	}
}

func TestClaudeHostEnd(t *testing.T) {
	root := t.TempDir()
	for _, authority := range []string{`{"project":"project-a","project_source":"config"}`, `{}`, `{"project":"project-a","project_source":"config","error_hint":"unsafe"}`} {
		t.Run(authority, func(t *testing.T) {
			closes := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/project/current" {
					_, _ = io.WriteString(w, authority)
					return
				}
				if r.Method != http.MethodPost || r.URL.EscapedPath() != "/sessions/host%2Fid/end" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["effective_continuation"] != true || body["ownership_mode"] != "project_owned" || body["project"] != "project-a" || body["directory"] != projectpkg.RuntimeWorktreeDirectory(root) {
					t.Errorf("unsafe close body: %v", body)
				}
				closes++
				_, _ = io.WriteString(w, `{"id":"host/id","status":"completed"}`)
			}))
			defer endpoint.Close()
			t.Setenv("ENGRAM_URL", endpoint.URL)
			input, _ := json.Marshal(map[string]string{"session_id": "host/id", "cwd": root})
			for i := 0; i < 2; i++ {
				err := endClaudeHookInput(input)
				if (err == nil) != (authority == `{"project":"project-a","project_source":"config"}`) {
					t.Fatalf("close error: %v", err)
				}
			}
			want := 0
			if authority == `{"project":"project-a","project_source":"config"}` {
				want = 2
			}
			if closes != want {
				t.Fatalf("closes=%d want=%d", closes, want)
			}
		})
	}
}

func TestClaudeHostEndRejectsRedirects(t *testing.T) {
	for _, stage := range []string{"authority", "close"} {
		for _, status := range []int{302, 303, 307, 308} {
			t.Run(stage+"/"+strconv.Itoa(status), func(t *testing.T) {
				var targets, closes atomic.Int64
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/project/current":
						if stage == "authority" {
							w.Header().Set("Location", "/target")
							w.WriteHeader(status)
							return
						}
						_, _ = io.WriteString(w, `{"project":"project-a","project_source":"config"}`)
					case "/sessions/host/end":
						closes.Add(1)
						w.Header().Set("Location", "/target")
						w.WriteHeader(status)
					case "/target":
						targets.Add(1)
						body, _ := io.ReadAll(r.Body)
						t.Errorf("redirect target reached: method=%s body=%s", r.Method, body)
						if stage == "authority" {
							_, _ = io.WriteString(w, `{"project":"project-a","project_source":"config"}`)
						} else {
							_, _ = io.WriteString(w, `{"id":"continuation","status":"completed"}`)
						}
					default:
						t.Errorf("unexpected request (including registration): %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer endpoint.Close()
				t.Setenv("ENGRAM_URL", endpoint.URL)
				input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": t.TempDir()})
				if err := endClaudeHookInput(input); err == nil {
					t.Error("redirect accepted as confirmed closure")
				}
				wantCloses := int64(1)
				if stage == "authority" {
					wantCloses = 0
				}
				if targets.Load() != 0 || closes.Load() != wantCloses {
					t.Errorf("target requests=%d close requests=%d, want 0/%d", targets.Load(), closes.Load(), wantCloses)
				}
			})
		}
	}
}

func TestClaudeHostEndAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"different opaque completed ID", `{"id":"opaque/\"%\u0000tail\n","status":"completed","extra":true}`, 200, true},
		{"no active session", `{"status":"no_active_session"}`, 200, true},
		{"trailing whitespace", "{\"id\":\"continuation\",\"status\":\"completed\"} \n\t", 200, true},
		{"exact body bound", `{"status":"no_active_session"}` + strings.Repeat(" ", (64<<10)-len(`{"status":"no_active_session"}`)), 200, true},
		{"oversized whitespace", `{"status":"no_active_session"}` + strings.Repeat(" ", 64<<10), 200, false},
		{"empty object", `{}`, 200, false},
		{"unknown status", `{"id":"host","status":"other"}`, 200, false},
		{"missing status", `{"id":"host"}`, 200, false},
		{"numeric status", `{"id":"host","status":1}`, 200, false},
		{"null status", `{"id":"host","status":null}`, 200, false},
		{"wrong status case", `{"id":"host","status":"Completed"}`, 200, false},
		{"wrong property case", `{"id":"host","Status":"completed"}`, 200, false},
		{"missing completed ID", `{"status":"completed"}`, 200, false},
		{"blank completed ID", `{"id":" \n","status":"completed"}`, 200, false},
		{"numeric ID", `{"id":1,"status":"completed"}`, 200, false},
		{"null ID", `{"id":null,"status":"completed"}`, 200, false},
		{"array ID", `{"id":["host"],"status":"completed"}`, 200, false},
		{"wrong ID property case", `{"ID":"host","status":"completed"}`, 200, false},
		{"inactive with ID", `{"id":"host","status":"no_active_session"}`, 200, false},
		{"inactive with null ID", `{"id":null,"status":"no_active_session"}`, 200, false},
		{"array root", `[{"status":"no_active_session"}]`, 200, false},
		{"null root", `null`, 200, false},
		{"multiple objects", `{"status":"no_active_session"}{}`, 200, false},
		{"trailing garbage", `{"status":"no_active_session"}garbage`, 200, false},
		{"malformed", `{`, 200, false},
		{"oversized", `{"status":"no_active_session","padding":"` + strings.Repeat("x", 64<<10) + `"}`, 200, false},
		{"created status code", `{"id":"host","status":"completed"}`, 201, false},
		{"accepted status code", `{"id":"host","status":"completed"}`, 202, false},
		{"no content", ``, 204, false},
		{"redirect without location", `{"status":"no_active_session"}`, 307, false},
		{"not found", `{"status":"no_active_session"}`, 404, false},
		{"ownership conflict", `{}`, 409, false},
		{"server failure", `{}`, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int64
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/project/current":
					_, _ = io.WriteString(w, `{"project":"project-a","project_source":"config"}`)
				case "/sessions/host/end":
					closes.Add(1)
					if r.Method != http.MethodPost {
						t.Errorf("close method=%s", r.Method)
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected request (including registration): %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer endpoint.Close()
			t.Setenv("ENGRAM_URL", endpoint.URL)
			input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": t.TempDir()})
			err := endClaudeHookInput(input)
			if (err == nil) != tc.valid || closes.Load() != 1 {
				t.Errorf("close error=%v requests=%d, want success=%t and one request", err, closes.Load(), tc.valid)
			}
		})
	}
}

func TestClaudeHostEndRejectsInvalidAuthorityResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing project", `{}`, 200},
		{"unsafe source", `{"project":"project-a","project_source":"unknown"}`, 200},
		{"error hint", `{"project":"project-a","project_source":"config","error_hint":"unsafe"}`, 200},
		{"multiple objects", `{"project":"project-a","project_source":"config"}{}`, 200},
		{"trailing garbage", `{"project":"project-a","project_source":"config"}garbage`, 200},
		{"null root", `null`, 200},
		{"array root", `[{"project":"project-a","project_source":"config"}]`, 200},
		{"oversized", `{"project":"project-a","project_source":"config","padding":"` + strings.Repeat("x", 64<<10) + `"}`, 200},
		{"created response", `{"project":"project-a","project_source":"config"}`, 201},
		{"unavailable", `{}`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/project/current" {
					t.Errorf("invalid authority allowed mutation: %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer endpoint.Close()
			t.Setenv("ENGRAM_URL", endpoint.URL)
			input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": t.TempDir()})
			if err := endClaudeHookInput(input); err == nil {
				t.Error("invalid authority response accepted")
			}
		})
	}
}

func TestClaudeHostEndDispatchRejectsInvalidAcknowledgement(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = io.WriteString(w, `{"project":"project-a","project_source":"config"}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/sessions/host/end" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, `{"status":"completed"}`)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	oldStdin, oldExit := os.Stdin, exitFunc
	t.Cleanup(func() { os.Stdin, exitFunc = oldStdin, oldExit })
	var codes []int
	exitFunc = func(code int) { codes = append(codes, code) }
	input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": t.TempDir()})
	os.Stdin = claudeHookStdin(t, string(input), false)
	cmdHook([]string{"claude-session-end"})
	if len(codes) != 1 || codes[0] != 1 {
		t.Fatalf("invalid acknowledgement exit codes=%v, want [1]", codes)
	}
}

func TestClaudeHostEndClosesExistingContinuationOnly(t *testing.T) {
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.StartSessionWithOwnershipMode("host", "project-a", projectpkg.RuntimeWorktreeDirectory(root), store.SessionOwnershipProjectOwned); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("host", "finished"); err != nil {
		t.Fatal(err)
	}
	effective, err := db.ResumeSessionWithOwnershipMode("host", "project-a", projectpkg.RuntimeWorktreeDirectory(root), store.SessionOwnershipProjectOwned)
	if err != nil {
		t.Fatal(err)
	}
	production := server.New(db, 0).Handler()
	authority := "foreign"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_ = json.NewEncoder(w).Encode(map[string]string{"project": authority, "project_source": "config"})
			return
		}
		if r.URL.Path == "/sessions" {
			t.Error("host end attempted registration")
		}
		production.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	input, _ := json.Marshal(map[string]string{"session_id": "host", "cwd": root})
	if err := endClaudeHookInput(input); err == nil {
		t.Fatal("foreign project closed continuation")
	}
	authority = "project-a"
	// Temp directories can share an enclosing Git checkout on this host.
	// Set a distinct stored ownership directory rather than assuming otherwise.
	canonical := projectpkg.RuntimeWorktreeDirectory(root)
	if _, err := db.DB().Exec(`UPDATE sessions SET directory = ?`, canonical+"-foreign"); err != nil {
		t.Fatal(err)
	}
	if err := endClaudeHookInput(input); err == nil {
		t.Fatal("foreign directory closed continuation")
	}
	live, err := db.GetSession(effective)
	if err != nil || live.EndedAt != nil {
		t.Fatalf("unsafe close mutated live continuation: %+v %v", live, err)
	}
	if _, err := db.DB().Exec(`UPDATE sessions SET directory = ?`, canonical); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := endClaudeHookInput(input); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"host", effective} {
		row, err := db.GetSession(id)
		if err != nil || row.EndedAt == nil {
			t.Fatalf("session %s reopened or unclosed: %+v %v", id, row, err)
		}
	}
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM sessions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("end created sessions: count=%d err=%v", count, err)
	}
}

func TestCmdHookClaudeHostEndRejectsMissingMetadata(t *testing.T) {
	oldStdin, oldExit := os.Stdin, exitFunc
	t.Cleanup(func() { os.Stdin, exitFunc = oldStdin, oldExit })
	exits := 0
	exitFunc = func(code int) {
		if code != 1 {
			t.Errorf("exit=%d", code)
		}
		exits++
	}
	os.Stdin = claudeHookStdin(t, `{"session_id":"host"}`, false)
	cmdHook([]string{"claude-session-end"})
	if exits != 1 {
		t.Fatalf("failure exits=%d", exits)
	}
}

func TestClaudeHostEndMissingMetadataSkipsNetwork(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unsafe metadata contacted server: %s", r.URL)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	for _, input := range []string{`{`, `{}`, `{"session_id":"host"}`, `{"session_id":42,"cwd":"/work"}`, `{"session_id":"host","cwd":" "}`} {
		if err := endClaudeHookInput([]byte(input)); err == nil {
			t.Fatalf("unsafe input accepted: %s", input)
		}
	}
}

func TestClaudeModelEndDeniedWithoutNetwork(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("model end contacted server: %s", r.URL)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	for i := 0; i < 2; i++ {
		response := guardClaudePreToolUse([]byte(`{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_session_end","tool_input":{"id":"model"}}`))
		if !strings.Contains(string(response), `"permissionDecision":"deny"`) || !strings.Contains(string(response), "host lifecycle") {
			t.Fatalf("model end must deny: %s", response)
		}
	}
}

func TestCmdHookClaudeSessionRegister(t *testing.T) {
	oldStdin, oldOutput, oldExit := os.Stdin, claudeHookOutput, exitFunc
	t.Cleanup(func() { os.Stdin, claudeHookOutput, exitFunc = oldStdin, oldOutput, oldExit })
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			if r.URL.Query().Get("cwd") != "/work" {
				t.Errorf("cwd: %s", r.URL)
			}
			_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["id"] != "host" || body["project"] != "project-a" || body["directory"] != "/work" || body["ownership_mode"] != "project_owned" || body["resume"] != true {
			t.Errorf("registration: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"opaque-continuation","status":"created","resumed_from":"host"}`))
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	var output []byte
	var exits int
	claudeHookOutput = func(data []byte) error { output = append(output, data...); return nil }
	exitFunc = func(code int) {
		exits++
		if code != 1 {
			t.Errorf("exit=%d", code)
		}
	}
	os.Stdin = claudeHookStdin(t, `{"session_id":"host","cwd":"/work","tool_input":{"id":"model"}}`, false)
	cmdHook([]string{"claude-session-register"})
	if string(output) != `{"id":"opaque-continuation"}` || exits != 0 {
		t.Fatalf("output=%s exits=%d", output, exits)
	}
	for _, input := range []string{`{`, `{}`, `{"session_id":"host","cwd":" "}`, `{"session_id":1,"cwd":"/work"}`} {
		output = nil
		os.Stdin = claudeHookStdin(t, input, false)
		cmdHook([]string{"claude-session-register"})
		if len(output) != 0 {
			t.Fatalf("failure fabricated output: %s", output)
		}
	}
	if exits != 4 {
		t.Fatalf("failure exits=%d", exits)
	}
}

func TestClaudeRegistrationRejectsUnsafeAuthority(t *testing.T) {
	for _, authority := range []string{`{}`, `{"project":"foreign"}`, `{"project":"project-a","project_source":"unknown"}`, `{"project":"project-a","project_source":"config","error_hint":"invalid config"}`} {
		t.Run(authority, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/project/current" {
					t.Errorf("unsafe authority registered: %s", r.URL)
					return
				}
				_, _ = w.Write([]byte(authority))
			}))
			defer endpoint.Close()
			t.Setenv("ENGRAM_URL", endpoint.URL)
			output, err := registerClaudeHookInput([]byte(`{"session_id":"host","cwd":"/work"}`))
			if err == nil || output != nil {
				t.Fatalf("unsafe project allowed: %s %v", output, err)
			}
		})
	}
}

func TestClaudeRegistrationRequiresMatchingCreatedResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", http.StatusServiceUnavailable, `{}`},
		{"mismatched id", http.StatusCreated, `{"id":"other","status":"created"}`},
		{"malformed", http.StatusCreated, `{`},
		{"missing id", http.StatusCreated, `{"status":"created"}`},
		{"blank id", http.StatusCreated, `{"id":" ","status":"created","resumed_from":"host"}`},
		{"foreign root", http.StatusCreated, `{"id":"continuation","status":"created","resumed_from":"foreign"}`},
		{"missing status", http.StatusCreated, `{"id":"host"}`},
		{"trailing data", http.StatusCreated, `{"id":"host","status":"created"}{}`},
		{"wrong id type", http.StatusCreated, `{"id":1,"status":"created"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/project/current" {
					_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			t.Setenv("ENGRAM_URL", server.URL)
			input := []byte(`{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{}}`)
			var result struct {
				HookSpecificOutput struct {
					PermissionDecision string         `json:"permissionDecision"`
					UpdatedInput       map[string]any `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(guardClaudePreToolUse(input), &result); err != nil || result.HookSpecificOutput.PermissionDecision != "deny" || result.HookSpecificOutput.UpdatedInput != nil {
				t.Fatalf("must deny unconfirmed registration: %+v, %v", result, err)
			}
		})
	}
}

func TestShouldCheckForUpdatesSkipsInternalHook(t *testing.T) {
	if shouldCheckForUpdates([]string{"hook", "claude-pre-tool-use"}) {
		t.Fatal("internal hook must not run the update check before emitting a Claude hook response")
	}
}

func TestCmdHookWritesTransformedResponse(t *testing.T) {
	oldStdin, oldOutput := os.Stdin, claudeHookOutput
	t.Cleanup(func() { os.Stdin, claudeHookOutput = oldStdin, oldOutput })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
			return
		}
		if r.URL.Path == "/sessions" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"claude-session","status":"created"}`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL)
	}))
	defer server.Close()
	t.Setenv("ENGRAM_URL", server.URL)
	os.Stdin = claudeHookStdin(t, `{"session_id":"claude-session","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{"title":"decision"}}`, false)
	var output []byte
	claudeHookOutput = func(response []byte) error { output = append([]byte(nil), response...); return nil }
	cmdHook([]string{"claude-pre-tool-use"})
	var response struct {
		HookSpecificOutput struct {
			UpdatedInput       map[string]any `json:"updatedInput"`
			PermissionDecision string         `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.HookSpecificOutput.UpdatedInput["session_id"] != "claude-session" || response.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("successful hook output = %s, %v", output, err)
	}
}

func TestClaudeAdapterPersistsWritesForDistinctSameWorktreeHosts(t *testing.T) {
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const project = "same-worktree"
	hosts := []string{"claude-host-one", "claude-host-two"}
	for _, host := range hosts {
		if err := db.CreateSession(host, project, root); err != nil {
			t.Fatal(err)
		}
	}
	production := server.New(db, 0).Handler()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = io.WriteString(w, `{"project":"same-worktree","project_source":"config"}`)
			return
		}
		production.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	oldStdin, oldOutput := os.Stdin, claudeHookOutput
	t.Cleanup(func() { os.Stdin, claudeHookOutput = oldStdin, oldOutput })
	mcpServer := mcp.NewServerWithConfig(db, mcp.MCPConfig{DefaultProject: project}, nil)
	want := map[string]map[string]int{
		hosts[0]: {"one first": 1, "one second": 1},
		hosts[1]: {"two first": 1, "two second": 1},
	}
	for _, step := range []struct{ host, title string }{
		{hosts[0], "one first"}, {hosts[1], "two first"},
		{hosts[0], "one second"}, {hosts[1], "two second"},
	} {
		request, _ := json.Marshal(map[string]any{
			"session_id": step.host, "cwd": root, "tool_name": "mcp__engram__mem_save",
			"tool_input": map[string]any{"title": step.title, "content": step.title, "project": project, "session_id": "foreign-model-session"},
		})
		os.Stdin = claudeHookStdin(t, string(request), false)
		var output []byte
		claudeHookOutput = func(data []byte) error { output = append([]byte(nil), data...); return nil }
		cmdHook([]string{"claude-pre-tool-use"})
		var hook struct {
			HookSpecificOutput struct {
				PermissionDecision string         `json:"permissionDecision"`
				UpdatedInput       map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(output, &hook); err != nil {
			t.Fatal(err)
		}
		bound := hook.HookSpecificOutput
		if bound.PermissionDecision == "deny" || bound.UpdatedInput["session_id"] != step.host || bound.UpdatedInput["project"] != project {
			t.Fatalf("host %s bound output = %s", step.host, output)
		}
		call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "mem_save", "arguments": bound.UpdatedInput}})
		result := mcpServer.HandleMessage(context.Background(), call)
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), `"isError":true`) || !strings.Contains(string(encoded), step.title) {
			t.Fatalf("host %s MCP result = %s, err=%v", step.host, encoded, err)
		}
	}
	all, err := db.AllObservations(project, "", 100)
	if err != nil || len(all) != 4 {
		t.Fatalf("project observations = %d, err=%v", len(all), err)
	}
	for host, expected := range want {
		observations, err := db.SessionObservations(host, 100)
		if err != nil || len(observations) != 2 {
			t.Fatalf("host %s observations = %v, err=%v", host, observations, err)
		}
		counts := make(map[string]int)
		for _, observation := range observations {
			counts[observation.Title]++
		}
		for title, count := range expected {
			if counts[title] != count {
				t.Fatalf("host %s title %q count = %d, want %d; all titles: %v", host, title, counts[title], count, counts)
			}
		}
		if len(counts) != len(expected) {
			t.Fatalf("host %s has unexpected titles: %v", host, counts)
		}
	}
	if _, err := db.GetSession("foreign-model-session"); err == nil {
		t.Fatal("foreign model session was created")
	}
}

func TestClaudeShellLifecyclePersistsOnlyLiveHostWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the Claude SessionStart bash hook")
	}
	for _, binary := range []string{"bash", "jq", "curl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("requires %s: %v", binary, err)
		}
	}
	root := t.TempDir()
	db, err := store.New(store.FallbackConfig(filepath.Join(root, "store")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const project = "same-worktree"
	hosts := []string{"shell-host-one", "shell-host-two"}
	const endedHost = "shell-host-ended"
	production := server.New(db, 0).Handler()
	var registrations, conflicts atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = io.WriteString(w, `{"project":"same-worktree","project_source":"config"}`)
			return
		}
		if r.URL.Path == "/sessions" && r.Method == http.MethodPost {
			registrations.Add(1)
			capture := &statusCapture{ResponseWriter: w}
			production.ServeHTTP(capture, r)
			if capture.status == http.StatusConflict {
				conflicts.Add(1)
			}
			return
		}
		production.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	stubDir := filepath.Join(root, "bin")
	if err := os.Mkdir(stubDir, 0700); err != nil {
		t.Fatal(err)
	}
	bashEnv := filepath.Join(stubDir, "engram-mock.sh")
	stub := `engram() {
  [ "$*" = 'hook claude-session-register' ] || return 1
  ENGRAM_TEST_CLAUDE_REGISTER_PROCESS=1 "$ENGRAM_TEST_EXECUTABLE" -test.run '^TestClaudeLifecycleRegistrationProcess$'
}
`
	if err := os.WriteFile(bashEnv, []byte(stub), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", filepath.ToSlash(bashEnv))
	t.Setenv("ENGRAM_TEST_EXECUTABLE", filepath.ToSlash(executable))
	t.Setenv("ENGRAM_URL", endpoint.URL)
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("ENGRAM_SOCKET", "")
	t.Setenv("ENGRAM_PROJECT", "")
	t.Setenv("ENGRAM_PORT", "")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	start := func(host string) {
		t.Helper()
		input, _ := json.Marshal(map[string]string{"session_id": host, "cwd": root})
		cmd := exec.Command("bash", filepath.Join(packageDir, "..", "..", "plugin", "claude-code", "scripts", "session-start.sh"))
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(string(input))
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("SessionStart %s: %v: %s", host, err, out)
		}
	}
	for _, host := range hosts {
		start(host)
		row, err := db.GetSession(host)
		if err != nil || row.EndedAt != nil {
			t.Fatalf("SessionStart %s did not persist live session: %+v, %v", host, row, err)
		}
	}
	if registered, conflicted := registrations.Load(), conflicts.Load(); registered != 2 || conflicted != 0 {
		t.Fatalf("initial registrations = %d, conflicts = %d", registered, conflicted)
	}
	oldStdin, oldOutput := os.Stdin, claudeHookOutput
	t.Cleanup(func() { os.Stdin, claudeHookOutput = oldStdin, oldOutput })
	mcpServer := mcp.NewServerWithConfig(db, mcp.MCPConfig{DefaultProject: project}, nil)
	dispatches := 0
	preToolUse := func(host, title string) (string, map[string]any) {
		t.Helper()
		request, _ := json.Marshal(map[string]any{"session_id": host, "cwd": root, "tool_name": "mcp__engram__mem_save", "tool_input": map[string]any{"title": title, "content": title, "project": project, "session_id": "foreign-model-session"}})
		os.Stdin = claudeHookStdin(t, string(request), false)
		var output []byte
		claudeHookOutput = func(data []byte) error { output = append([]byte(nil), data...); return nil }
		cmdHook([]string{"claude-pre-tool-use"})
		var hook struct {
			HookSpecificOutput struct {
				PermissionDecision string         `json:"permissionDecision"`
				UpdatedInput       map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(output, &hook); err != nil {
			t.Fatalf("hook response %s: %v", output, err)
		}
		return hook.HookSpecificOutput.PermissionDecision, hook.HookSpecificOutput.UpdatedInput
	}
	want := map[string]map[string]int{hosts[0]: {"one first": 1, "one second": 1}, hosts[1]: {"two first": 1, "two second": 1}}
	for _, step := range []struct{ host, title string }{{hosts[0], "one first"}, {hosts[1], "two first"}, {hosts[0], "one second"}, {hosts[1], "two second"}} {
		decision, bound := preToolUse(step.host, step.title)
		if decision == "deny" || bound["session_id"] != step.host || bound["project"] != project {
			t.Fatalf("host %s decision %q, bound = %v", step.host, decision, bound)
		}
		call, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "mem_save", "arguments": bound}})
		dispatches++
		result := mcpServer.HandleMessage(context.Background(), call)
		encoded, err := json.Marshal(result)
		if err != nil || strings.Contains(string(encoded), `"isError":true`) || !strings.Contains(string(encoded), step.title) {
			t.Fatalf("MCP result = %s, err = %v", encoded, err)
		}
	}
	if err := db.CreateSession(endedHost, project, root); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(endedHost, "finished"); err != nil {
		t.Fatal(err)
	}
	start(endedHost)
	if registered, conflicted := registrations.Load(), conflicts.Load(); registered != 7 || conflicted != 0 {
		t.Fatalf("registrations = %d, production 409s = %d", registered, conflicted)
	}
	decision, bound := preToolUse(endedHost, "must not persist")
	if decision == "deny" || bound["session_id"] != endedHost+":resume:2" || dispatches != 4 {
		t.Fatalf("resumed host decision %q, bound %v, dispatches %d", decision, bound, dispatches)
	}
	all, err := db.AllObservations(project, "", 100)
	if err != nil || len(all) != 4 {
		t.Fatalf("observations = %d, err = %v", len(all), err)
	}
	for host, expected := range want {
		observations, err := db.SessionObservations(host, 100)
		if err != nil || len(observations) != 2 {
			t.Fatalf("host %s observations = %v, err = %v", host, observations, err)
		}
		counts := map[string]int{}
		for _, observation := range observations {
			counts[observation.Title]++
		}
		if len(counts) != len(expected) {
			t.Fatalf("host %s titles = %v", host, counts)
		}
		for title, count := range expected {
			if counts[title] != count {
				t.Fatalf("host %s title %q count = %d", host, title, counts[title])
			}
		}
	}
	ended, err := db.GetSession(endedHost)
	if err != nil || ended.EndedAt == nil {
		t.Fatalf("ended row = %+v, err = %v", ended, err)
	}
	endedWrites, err := db.SessionObservations(endedHost, 100)
	if err != nil || len(endedWrites) != 0 {
		t.Fatalf("ended writes = %v, err = %v", endedWrites, err)
	}
	if _, err := db.GetSession("foreign-model-session"); err == nil {
		t.Fatal("foreign model session was created")
	}
}

type statusCapture struct {
	http.ResponseWriter
	status int
}

func (w *statusCapture) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func TestCmdHookExitsWhenClaudeResponseWriteFails(t *testing.T) {
	oldStdin, oldOutput, oldExit := os.Stdin, claudeHookOutput, exitFunc
	t.Cleanup(func() { os.Stdin, claudeHookOutput, exitFunc = oldStdin, oldOutput, oldExit })
	claudeHookOutput = func([]byte) error { return errors.New("write Claude hook response") }
	var exitCodes []int
	exitFunc = func(code int) { exitCodes = append(exitCodes, code) }
	os.Stdin = claudeHookStdin(t, `{"session_id":"claude-session","tool_name":"mcp__engram__mem_save","tool_input":{"title":"decision"}}`, false)
	cmdHook([]string{"claude-pre-tool-use"})
	os.Stdin = claudeHookStdin(t, "", true)
	cmdHook([]string{"claude-pre-tool-use"})
	if len(exitCodes) != 2 || exitCodes[0] != 1 || exitCodes[1] != 1 {
		t.Fatalf("exit codes = %v, want [1 1] after Claude hook response write failures", exitCodes)
	}
}

func TestCmdHookEmitsJSONDenialWhenClaudeInputReadFails(t *testing.T) {
	oldStdin, oldOutput := os.Stdin, claudeHookOutput
	t.Cleanup(func() { os.Stdin, claudeHookOutput = oldStdin, oldOutput })
	os.Stdin = claudeHookStdin(t, "", true)
	var output []byte
	claudeHookOutput = func(response []byte) error { output = append([]byte(nil), response...); return nil }
	cmdHook([]string{"claude-pre-tool-use"})
	var response struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("command output = %q, want JSON denial: %v", output, err)
	}
	if response.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hook event = %q, want PreToolUse", response.HookSpecificOutput.HookEventName)
	}
	if response.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("permissionDecision = %q, want deny", response.HookSpecificOutput.PermissionDecision)
	}
	if response.HookSpecificOutput.PermissionDecisionReason != "cannot read authoritative Claude hook input" {
		t.Fatalf("permissionDecisionReason = %q", response.HookSpecificOutput.PermissionDecisionReason)
	}
}

func TestCodexPreToolUseCommandDeniesUnreadableInput(t *testing.T) {
	oldStdin, oldOutput, oldExit := os.Stdin, claudeHookOutput, exitFunc
	t.Cleanup(func() { os.Stdin, claudeHookOutput, exitFunc = oldStdin, oldOutput, oldExit })
	os.Stdin = claudeHookStdin(t, "", true)
	var output []byte
	claudeHookOutput = func(response []byte) error { output = append([]byte(nil), response...); return nil }
	exitFunc = func(code int) { t.Errorf("unexpected exit code %d", code) }
	cmdHook([]string{"codex-pre-tool-use"})
	var response struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("command output = %q, want JSON denial: %v", output, err)
	}
	if got := response.HookSpecificOutput; got.HookEventName != "PreToolUse" || got.PermissionDecision != "deny" || got.PermissionDecisionReason != "cannot read authoritative Codex hook input" {
		t.Fatalf("Codex read failure response = %+v", got)
	}
}

func TestCodexPreToolUseCommandExitsWhenResponseWriteFails(t *testing.T) {
	oldStdin, oldOutput, oldExit := os.Stdin, claudeHookOutput, exitFunc
	t.Cleanup(func() { os.Stdin, claudeHookOutput, exitFunc = oldStdin, oldOutput, oldExit })
	os.Stdin = claudeHookStdin(t, `{"session_id":"host","tool_name":"mcp__engram__mem_save","tool_input":{"title":"decision"}}`, false)
	claudeHookOutput = func([]byte) error { return errors.New("write Codex hook response") }
	var exitCodes []int
	exitFunc = func(code int) { exitCodes = append(exitCodes, code) }
	cmdHook([]string{"codex-pre-tool-use"})
	if len(exitCodes) != 1 || exitCodes[0] != 1 {
		t.Fatalf("exit codes = %v, want [1] after Codex response write failure", exitCodes)
	}
}

func TestCodexPreToolUseNamespacedSaveUsesHostID(t *testing.T) {
	for _, tc := range []struct {
		name, input, decision string
	}{
		{"model ID replaced", `{"session_id":"host","tool_name":"mcp__plugin_engram_engram__mem_save","tool_input":{"session_id":"model-picked","title":"retained"}}`, "allow"},
		{"missing host ID denied", `{"tool_name":"mcp__plugin_engram_engram__mem_save","tool_input":{"session_id":"model-picked"}}`, "deny"},
		{"malformed input denied", `{"session_id":"host","tool_name":"mcp__plugin_engram_engram__mem_save","tool_input":null}`, "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response struct {
				HookSpecificOutput struct {
					PermissionDecision string         `json:"permissionDecision"`
					UpdatedInput       map[string]any `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(transformCodexPreToolUse([]byte(tc.input)), &response); err != nil {
				t.Fatal(err)
			}
			if got := response.HookSpecificOutput.PermissionDecision; got != tc.decision {
				t.Fatalf("decision = %q, want %q", got, tc.decision)
			}
			if tc.decision == "allow" && (response.HookSpecificOutput.UpdatedInput["session_id"] != "host" || response.HookSpecificOutput.UpdatedInput["title"] != "retained") {
				t.Fatalf("unexpected bound input: %#v", response.HookSpecificOutput.UpdatedInput)
			}
		})
	}
}

func TestCodexPreToolUseBindsWrites(t *testing.T) {
	for _, tool := range claudeEngramWriteAndSessionTools {
		t.Run(tool, func(t *testing.T) {
			field := "session_id"
			if tool == "mem_session_start" || tool == "mem_session_end" {
				field = "id"
			}
			input := []byte(`{"session_id":"host","tool_name":"mcp__engram__` + tool + `","tool_input":{"` + field + `":"model-picked","other":{"keep":true}}}`)
			var response struct {
				HookSpecificOutput struct {
					PermissionDecision string         `json:"permissionDecision"`
					UpdatedInput       map[string]any `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(transformCodexPreToolUse(input), &response); err != nil {
				t.Fatal(err)
			}
			if response.HookSpecificOutput.PermissionDecision != "allow" || response.HookSpecificOutput.UpdatedInput[field] != "host" || response.HookSpecificOutput.UpdatedInput["other"].(map[string]any)["keep"] != true {
				t.Fatalf("unexpected Codex rewrite: %+v", response)
			}
		})
	}
}

func TestCodexPreToolUseRejectsMalformedInputAndLeavesReadsUntouched(t *testing.T) {
	for _, input := range []string{
		`not json`,
		`{"tool_name":"mcp__engram__mem_save","tool_input":{}}`,
		`{"session_id":"host","tool_name":"mcp__engram__mem_save","tool_input":null}`,
		`{"session_id":"host","tool_name":"mcp__engram__mem_save","tool_input":[]}`,
	} {
		t.Run(input, func(t *testing.T) {
			var response struct {
				HookSpecificOutput struct {
					PermissionDecision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal(transformCodexPreToolUse([]byte(input)), &response); err != nil || response.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("malformed input response: %+v, %v", response, err)
			}
		})
	}
	for _, tool := range []string{"mcp__engram__mem_search", "mcp__other__mem_save"} {
		input := `{"session_id":"host","tool_name":"` + tool + `","tool_input":{"session_id":"model"}}`
		if got := string(transformCodexPreToolUse([]byte(input))); got != "{}" {
			t.Errorf("non-write response for %s = %s", tool, got)
		}
	}
}

func TestCodexPreToolUseInterleavedHostSessionsRemainDistinct(t *testing.T) {
	for _, host := range []string{"host-one", "host-two", "host-one"} {
		input := []byte(`{"session_id":"` + host + `","tool_name":"mcp__engram__mem_save","tool_input":{"session_id":"model-picked","project":"same-project","directory":"same-directory"}}`)
		var response struct {
			HookSpecificOutput struct {
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(transformCodexPreToolUse(input), &response); err != nil {
			t.Fatal(err)
		}
		if got := response.HookSpecificOutput.UpdatedInput; got["session_id"] != host || got["project"] != "same-project" || got["directory"] != "same-directory" {
			t.Fatalf("host %q updated input = %#v", host, got)
		}
	}
}

func TestCodexPreToolUseReadsDoNotRequireSessionOrInput(t *testing.T) {
	for _, input := range []string{
		`{"tool_name":"mcp__engram__mem_search"}`,
		`{"session_id":42,"tool_name":"mcp__engram__mem_context","tool_input":null}`,
		`{"tool_name":"mcp__other__mem_save","tool_input":[]}`,
	} {
		if got := string(transformCodexPreToolUse([]byte(input))); got != "{}" {
			t.Errorf("read/non-Engram call %s = %s, want {}", input, got)
		}
	}
}

func TestCodexPreToolUseCommandWritesAllowAndBoundInput(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/project/current" {
			_, _ = w.Write([]byte(`{"project":"project-a","project_source":"config"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"host","status":"created"}`))
	}))
	defer endpoint.Close()
	t.Setenv("ENGRAM_URL", endpoint.URL)
	oldStdin, oldOutput := os.Stdin, claudeHookOutput
	t.Cleanup(func() { os.Stdin, claudeHookOutput = oldStdin, oldOutput })
	os.Stdin = claudeHookStdin(t, `{"session_id":"host","cwd":"/work","tool_name":"mcp__engram__mem_save","tool_input":{"session_id":"model","title":"retained"}}`, false)
	var output []byte
	claudeHookOutput = func(response []byte) error { output = append([]byte(nil), response...); return nil }
	cmdHook([]string{"codex-pre-tool-use"})
	var response struct {
		HookSpecificOutput struct {
			PermissionDecision string         `json:"permissionDecision"`
			UpdatedInput       map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.HookSpecificOutput.PermissionDecision != "allow" || response.HookSpecificOutput.UpdatedInput["session_id"] != "host" || response.HookSpecificOutput.UpdatedInput["title"] != "retained" {
		t.Fatalf("command response = %s, %v", output, err)
	}
}

func TestTransformClaudePreToolUseBindsEngramWritesToAuthoritativeSession(t *testing.T) {
	input := []byte(`{
		"session_id":"claude-parent-session",
		"tool_name":"mcp__engram__mem_save",
		"tool_input":{"title":"decision","content":"keep this","session_id":"model-invented","project":"engram","nested":{"keep":true}}
	}`)

	output := transformClaudePreToolUse(input)
	var response struct {
		HookSpecificOutput struct {
			HookEventName      string         `json:"hookEventName"`
			UpdatedInput       map[string]any `json:"updatedInput"`
			PermissionDecision string         `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode hook response: %v\n%s", err, output)
	}
	if response.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hook event = %q, want PreToolUse", response.HookSpecificOutput.HookEventName)
	}
	if response.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("successful rewrite must not auto-allow, got permissionDecision=%q", response.HookSpecificOutput.PermissionDecision)
	}
	updated := response.HookSpecificOutput.UpdatedInput
	if got := updated["session_id"]; got != "claude-parent-session" {
		t.Fatalf("session_id = %#v, want authoritative Claude session", got)
	}
	if got := updated["title"]; got != "decision" {
		t.Fatalf("title = %#v, want preserved", got)
	}
	if got := updated["project"]; got != "engram" {
		t.Fatalf("project = %#v, want preserved", got)
	}
	nested, ok := updated["nested"].(map[string]any)
	if !ok || nested["keep"] != true {
		t.Fatalf("nested arguments = %#v, want preserved", updated["nested"])
	}
}

func TestTransformClaudePreToolUseBindsEveryEngramWriteAndSessionTool(t *testing.T) {
	for _, tool := range claudeEngramWriteAndSessionTools {
		for _, server := range []string{"mcp__engram__", "mcp__plugin_engram_engram__"} {
			t.Run(server+tool, func(t *testing.T) {
				bindingField := "session_id"
				if tool == "mem_session_start" || tool == "mem_session_end" {
					bindingField = "id"
				}
				input := []byte(`{"session_id":"subagent-session","tool_name":"` + server + tool + `","tool_input":{"` + bindingField + `":"wrong","value":"preserved"}}`)
				output := transformClaudePreToolUse(input)
				var response struct {
					HookSpecificOutput struct {
						UpdatedInput map[string]any `json:"updatedInput"`
					} `json:"hookSpecificOutput"`
				}
				if err := json.Unmarshal(output, &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if got := response.HookSpecificOutput.UpdatedInput[bindingField]; got != "subagent-session" {
					t.Fatalf("%s = %#v, want subagent authoritative session", bindingField, got)
				}
				if got := response.HookSpecificOutput.UpdatedInput["value"]; got != "preserved" {
					t.Fatalf("unrelated tool input = %#v, want preserved", got)
				}
			})
		}
	}
}

func TestTransformClaudePreToolUseLeavesNonEngramAndReadToolsUntouched(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"session_id":"claude-session","tool_name":"mcp__other__mem_save","tool_input":{"session_id":"model"}}`),
		[]byte(`{"session_id":"claude-session","tool_name":"mcp__engram__mem_search","tool_input":{"query":"history"}}`),
	} {
		if got := strings.TrimSpace(string(transformClaudePreToolUse(input))); got != "{}" {
			t.Fatalf("non-target tool response = %s, want {}", got)
		}
	}
}

func TestTransformClaudePreToolUseFailsClosedForMalformedAuthoritativeInput(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"tool_name":"mcp__engram__mem_save","tool_input":{}}`),
		[]byte(`{"session_id":" ","tool_name":"mcp__engram__mem_save","tool_input":{}}`),
		[]byte(`{"session_id":"claude-session","tool_name":"mcp__engram__mem_save","tool_input":null}`),
		[]byte(`{"session_id":"claude-session","tool_name":42,"tool_input":{}}`),
	} {
		output := transformClaudePreToolUse(input)
		var response struct {
			HookSpecificOutput struct {
				HookEventName      string `json:"hookEventName"`
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(output, &response); err != nil {
			t.Fatalf("decode denial: %v\n%s", err, output)
		}
		if response.HookSpecificOutput.HookEventName != "PreToolUse" || response.HookSpecificOutput.PermissionDecision != "deny" {
			t.Fatalf("malformed authoritative input response = %#v, want PreToolUse deny", response.HookSpecificOutput)
		}
	}
}
