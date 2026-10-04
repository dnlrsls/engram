package plugin_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestClaudeCodeWindowsPromptResolverRejectsMalformedCanonicalProject(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")

	for _, test := range []struct {
		name       string
		status     int
		resolution string
	}{
		{name: "non-string project", status: http.StatusOK, resolution: `{"project":42,"project_source":"config"}`},
		{name: "malformed JSON", status: http.StatusOK, resolution: `not-json`},
		{name: "non-string project source", status: http.StatusOK, resolution: `{"project":"canonical-project","project_source":42}`},
		{name: "incorrectly cased project property", status: http.StatusOK, resolution: `{"Project":"canonical-project","project_source":"config"}`},
		{name: "incorrectly cased project source property", status: http.StatusOK, resolution: `{"project":"canonical-project","Project_Source":"config"}`},
		{name: "incorrectly cased project source value", status: http.StatusOK, resolution: `{"project":"canonical-project","project_source":"CONFIG"}`},
		{name: "blank project", status: http.StatusOK, resolution: `{"project":"","project_source":"config"}`},
		{name: "error hint", status: http.StatusOK, resolution: `{"project":"canonical-project","project_source":"config","error_hint":"choose a project"}`},
		{name: "ambiguous response", status: http.StatusOK, resolution: `{"project":"","project_source":"ambiguous","available_projects":["one","two"]}`},
		{name: "non-2xx response", status: http.StatusServiceUnavailable, resolution: `unavailable`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			resolutionRequests := 0
			promptWrites := 0
			requestedCWD := ""
			cwd := "C:/workspace/reserved &?#%+"
			port := claudeCodeWindowsPromptServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()

				switch r.URL.Path {
				case "/project/current":
					resolutionRequests++
					requestedCWD = r.URL.Query().Get("cwd")
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.resolution))
				case "/prompts":
					promptWrites++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusNotFound)
				}
			}))

			runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, "resolver-malformed", cwd, "persist this prompt", true)

			mu.Lock()
			defer mu.Unlock()
			if resolutionRequests != 1 {
				t.Fatalf("canonical resolution requests = %d, want 1", resolutionRequests)
			}
			if requestedCWD != cwd {
				t.Fatalf("canonical request cwd = %q, want %q", requestedCWD, cwd)
			}
			if promptWrites != 0 {
				t.Fatalf("prompt writes = %d, want 0 for malformed canonical project", promptWrites)
			}
		})
	}
}

func TestClaudeCodeWindowsPromptResolverPersistsCanonicalProject(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")

	var mu sync.Mutex
	resolutionRequests := 0
	promptWrites := 0
	requestedCWD := ""
	cwd := "C:/workspace/reserved &?#%+"
	var promptPayload struct {
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
		Content   string `json:"content"`
	}
	port := claudeCodeWindowsPromptServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/project/current":
			resolutionRequests++
			requestedCWD = r.URL.Query().Get("cwd")
			_, _ = w.Write([]byte(`{"project":"canonical-project","project_source":"config"}`))
		case "/prompts":
			promptWrites++
			if r.Method != http.MethodPost {
				t.Errorf("prompt method = %s, want POST", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&promptPayload); err != nil {
				t.Errorf("decode prompt payload: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, "canonical-persistence", cwd, "persist this prompt", true)

	mu.Lock()
	defer mu.Unlock()
	if resolutionRequests != 1 {
		t.Fatalf("canonical resolution requests = %d, want 1", resolutionRequests)
	}
	if requestedCWD != cwd {
		t.Fatalf("canonical request cwd = %q, want %q", requestedCWD, cwd)
	}
	if promptWrites != 1 {
		t.Fatalf("prompt writes = %d, want 1", promptWrites)
	}
	if promptPayload.SessionID != "canonical-persistence" || promptPayload.Project != "canonical-project" || promptPayload.Content != "persist this prompt" {
		t.Fatalf("prompt payload = %+v, want canonical project-bearing payload", promptPayload)
	}
}

func TestClaudeCodeWindowsPromptBootstrapOutput(t *testing.T) {
	powershellPath := claudeCodePowerShell(t)
	adapterPath := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")
	port := claudeCodeWindowsPromptServer(t, http.NotFoundHandler())
	const sessionID = "windows-bootstrap-output"

	first := decodeHookPayload(t, runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, sessionID, t.TempDir(), "first message", true))
	if got := first.HookSpecificOutput.HookEventName; got != "UserPromptSubmit" {
		t.Errorf("first message hookEventName = %q, want %q", got, "UserPromptSubmit")
	}
	assertToolSearchNames(t, selectNames(t, first.HookSpecificOutput.AdditionalContext))

	secondOutput := runClaudeCodeWindowsPromptHook(t, powershellPath, adapterPath, port, sessionID, t.TempDir(), "second message", false)
	if got := strings.TrimSpace(secondOutput); got != "{}" {
		t.Errorf("second message response = %q, want {}", got)
	}
	second := decodeHookPayload(t, secondOutput)
	if second.HookSpecificOutput.AdditionalContext != "" {
		t.Errorf("bootstrap fired twice for one session: %q", second.HookSpecificOutput.AdditionalContext)
	}
}

// Canonical fixtures are independent of the PowerShell trim implementation.
func TestClaudeBridgeOwnershipPowerShell(t *testing.T) {
	if testing.Short() {
		t.Skip("external PowerShell hook integration")
	}
	ps := claudeCodePowerShell(t)
	adapter := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1")
	for _, tc := range []struct {
		name, prompt, canonical, mutation string
		owned                             bool
	}{
		{"valid", "original prompt", "original prompt", "", true},
		{"short fallback", "short", "short", "unset", false},
		{"unset", "original prompt", "original prompt", "unset", false},
		{"Pi alone", "original prompt", "original prompt", "pi", false},
		{"registration only", "original prompt", "original prompt", "registration", false},
		{"wrong nested session", "original prompt", "original prompt", "child", false},
		{"malformed", "original prompt", "original prompt", "malformed", false},
		{"array object", "original prompt", "original prompt", "array", false},
		{"null", "original prompt", "original prompt", "null", false},
		{"trailing JSON", "original prompt", "original prompt", "trailing", false},
		{"JSON comment", "original prompt", "original prompt", "comment", false},
		{"JSON trailing comma", "original prompt", "original prompt", "comma", false},
		{"version property casing", "original prompt", "original prompt", "version case", false},
		{"raw session casing", "original prompt", "original prompt", "payload session case", false},
		{"raw prompt casing", "original prompt", "original prompt", "payload prompt case", false},
		{"FEFF nonblank identity", "original prompt", "original prompt", "FEFF nonblank", true},
		{"runtime wrong type", "original prompt", "original prompt", "runtime type", false},
		{"runtime blank", "original prompt", "original prompt", "blank", false},
		{"claude wrong type", "original prompt", "original prompt", "claude type", false},
		{"property casing", "original prompt", "original prompt", "property case", false},
		{"digest property casing", "original prompt", "original prompt", "digest case", false},
		{"version bool", "original prompt", "original prompt", "bool", false},
		{"version string", "original prompt", "original prompt", "string", false},
		{"version unknown", "original prompt", "original prompt", "version", false},
		{"digest mismatch", "original prompt", "different prompt", "", false},
		{"digest uppercase", "original prompt", "original prompt", "uppercase", false},
		{"digest type", "original prompt", "original prompt", "digest type", false},
		{"missing digest", "original prompt", "original prompt", "missing", false},
		{"digest newline", "original prompt", "original prompt", "digest newline", false},
		{"session ordinal", "original prompt", "original prompt", "session case", false},
		{"numeric session", "original prompt", "original prompt", "numeric session", false},
		{"numeric prompt", "42", "42", "numeric prompt", false},
		{"missing session", "original prompt", "original prompt", "missing session", false},
		{"FEFF identity", "original prompt", "original prompt", "FEFF identity", false},
		{"ECMAScript whitespace", "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufefforiginal prompt\ufeff\n", "original prompt", "", true},
		{"NEL retained", "\u0085original prompt\u0085", "\u0085original prompt\u0085", "", true},
		{"MVS retained", "\u180eoriginal prompt\u180e", "\u180eoriginal prompt\u180e", "", true},
		{"zero width retained", "\u200boriginal prompt\u200b", "\u200boriginal prompt\u200b", "", true},
		{"NEL not trimmed", "\u0085original prompt\u0085", "original prompt", "", false},
		{"LF and NUL", "first\nsecond\x00third\n", "first\nsecond\x00third", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sum := sha256.Sum256([]byte(tc.canonical))
			marker := map[string]any{"version": 1, "runtimeSessionId": "pi-runtime", "claudeSessionId": "parent", "promptDigest": hex.EncodeToString(sum[:])}
			payload := map[string]any{"session_id": "parent", "cwd": root, "prompt": tc.prompt}
			switch tc.mutation {
			case "child":
				marker["claudeSessionId"] = "nested-child"
			case "runtime type":
				marker["runtimeSessionId"] = 42
			case "claude type":
				marker["claudeSessionId"] = 42
			case "blank":
				marker["runtimeSessionId"] = " \t"
			case "FEFF identity":
				marker["runtimeSessionId"] = "\ufeff"
			case "FEFF nonblank":
				marker["runtimeSessionId"] = "\ufeffpi-runtime\ufeff"
			case "version case":
				marker["Version"] = marker["version"]
				delete(marker, "version")
			case "payload session case":
				payload["Session_ID"] = payload["session_id"]
				delete(payload, "session_id")
			case "payload prompt case":
				payload["Prompt"] = payload["prompt"]
				delete(payload, "prompt")
			case "property case":
				marker["RuntimeSessionId"] = marker["runtimeSessionId"]
				delete(marker, "runtimeSessionId")
			case "digest case":
				marker["PromptDigest"] = marker["promptDigest"]
				delete(marker, "promptDigest")
			case "bool":
				marker["version"] = true
			case "string":
				marker["version"] = "1"
			case "version":
				marker["version"] = 2
			case "uppercase":
				marker["promptDigest"] = strings.ToUpper(marker["promptDigest"].(string))
			case "digest type":
				marker["promptDigest"] = 42
			case "missing":
				delete(marker, "promptDigest")
			case "digest newline":
				marker["promptDigest"] = marker["promptDigest"].(string) + "\n"
			case "session case":
				marker["claudeSessionId"] = "PARENT"
			case "numeric session":
				payload["session_id"] = 42
				marker["claudeSessionId"] = "42"
			case "numeric prompt":
				payload["prompt"] = 42
			case "missing session":
				delete(payload, "session_id")
			}
			encoded, _ := json.Marshal(marker)
			claim := string(encoded)
			switch tc.mutation {
			case "malformed":
				claim = "{"
			case "array":
				claim = "[" + claim + "]"
			case "null":
				claim = "null"
			case "trailing":
				claim += " {}"
			case "comment":
				claim = "/*not JSON*/" + claim
			case "comma":
				claim = strings.TrimSuffix(claim, "}") + ",}"
			}
			claims := map[string]string{"PI_ENGRAM_BRIDGE_PROMPT_CAPTURE": claim}
			if tc.mutation == "unset" || tc.mutation == "pi" || tc.mutation == "registration" {
				claims["PI_ENGRAM_BRIDGE_PROMPT_CAPTURE"] = ""
			}
			if tc.mutation == "pi" {
				claims["PI_CODING_AGENT"] = "1"
			}
			if tc.mutation == "registration" {
				claims["PI_ENGRAM_BRIDGE_SESSION_REGISTER"] = claim
			}
			var mu sync.Mutex
			var bodies []map[string]any
			resolutions := 0
			port := claudeCodeWindowsPromptServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/project/current":
					resolutions++
					if r.URL.Query().Get("cwd") != root {
						t.Error("canonical cwd changed")
					}
					if _, err := w.Write([]byte(`{"project":"canonical-project","project_source":"config"}`)); err != nil {
						t.Errorf("write canonical project response: %v", err)
					}
				case "/prompts":
					if r.Method != http.MethodPost {
						t.Error("expected POST")
					}
					body := map[string]any{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					bodies = append(bodies, body)
					w.WriteHeader(201)
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			input, _ := json.Marshal(payload)
			first := decodeHookPayload(t, runOwnedWindowsPrompt(t, ps, adapter, port, root, string(input), claims))
			assertToolSearchNames(t, selectNames(t, first.HookSpecificOutput.AdditionalContext))
			// Synthetic IDs are intentionally process-local; reuse checks require a real ID.
			runs := 1
			if tc.mutation != "missing session" {
				runs++
				if got := strings.TrimSpace(runOwnedWindowsPrompt(t, ps, adapter, port, root, string(input), claims)); got != "{}" {
					t.Fatalf("second bootstrap: %s", got)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := runs
			if tc.owned {
				want = 0
			}
			if len(bodies) != want || resolutions != runs {
				t.Fatalf("POSTs=%d resolutions=%d want %d/%d", len(bodies), resolutions, want, runs)
			}
			for _, body := range bodies {
				sid := "parent"
				if tc.mutation == "numeric session" {
					sid = "42"
				}
				if tc.mutation == "missing session" {
					if !strings.HasPrefix(body["session_id"].(string), "windows-") {
						t.Fatal("missing synthetic fallback")
					}
				} else if body["session_id"] != sid {
					t.Fatalf("session body: %v", body)
				}
				if body["content"] != tc.prompt || body["project"] != "canonical-project" {
					t.Fatalf("prompt body: %v", body)
				}
			}
		})
	}
}

func claudeCodeWindowsPromptServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on IPv4 loopback: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	return strconv.Itoa(tcpAddr.Port)
}

func runClaudeCodeWindowsPromptHook(t *testing.T, powershellPath, adapterPath, port, sessionID, cwd, prompt string, resetState bool) string {
	t.Helper()
	if resetState {
		t.Setenv("ENGRAM_TEST_PS_ROOT", t.TempDir())
	}
	root := os.Getenv("ENGRAM_TEST_PS_ROOT")
	if root == "" {
		t.Fatal("missing owned PowerShell fixture root")
	}
	input, err := json.Marshal(map[string]string{
		"session_id": sessionID,
		"cwd":        cwd,
		"prompt":     prompt,
	})
	if err != nil {
		t.Fatalf("marshal prompt hook input: %v", err)
	}
	return runOwnedWindowsPrompt(t, powershellPath, adapterPath, port, root, string(input), nil)
}

func runOwnedWindowsPrompt(t *testing.T, powershellPath, adapterPath, port, root, input string, claims map[string]string) string {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("unsafe fixture root %q: %v", root, err)
	}
	env := map[string]string{"HOME": root, "USERPROFILE": root, "TEMP": root, "TMP": root, "TMPDIR": root, "PI_CODING_AGENT_DIR": root, "CLAUDE_CONFIG_DIR": root, "ENGRAM_DATA_DIR": root, "ENGRAM_TEST_PS_ROOT": root, "ENGRAM_TEST_PS_ADAPTER": adapterPath, "ENGRAM_PORT": port}
	for key, value := range claims {
		env[key] = value
	}
	// Validate effective .NET temp/Pi paths before executing the hook; no deletion.
	guard := `$root = [IO.Path]::GetFullPath($env:ENGRAM_TEST_PS_ROOT).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar;
foreach ($path in @([IO.Path]::GetTempPath(), $env:PI_CODING_AGENT_DIR, $env:CLAUDE_CONFIG_DIR, $env:ENGRAM_DATA_DIR)) {
  $full = [IO.Path]::GetFullPath($path).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar;
  if (-not $full.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) { throw 'fixture path escaped owned root' }
}
& $env:ENGRAM_TEST_PS_ADAPTER`
	run := exec.Command(powershellPath, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", guard)
	for _, entry := range os.Environ() {
		key := strings.SplitN(entry, "=", 2)[0]
		if strings.HasPrefix(strings.ToUpper(key), "PI_ENGRAM_BRIDGE_") || strings.EqualFold(key, "PI_CODING_AGENT") {
			continue
		}
		overridden := false
		for override := range env {
			if strings.EqualFold(key, override) {
				overridden = true
				break
			}
		}
		if !overridden {
			run.Env = append(run.Env, entry)
		}
	}
	for key, value := range env {
		run.Env = append(run.Env, key+"="+value)
	}
	run.Stdin = strings.NewReader(input)
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("run isolated UserPromptSubmit: %v: %s", err, output)
	}
	return string(output)
}

func claudeCodePowerShell(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"pwsh", "powershell.exe"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("requires PowerShell")
	return ""
}
