package plugin_test

import (
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
	stateFile := filepath.Join(os.TempDir(), "engram-claude-"+sessionID+"-tools-loaded")
	if resetState {
		_ = os.Remove(stateFile)
		t.Cleanup(func() { _ = os.Remove(stateFile) })
	}

	input, err := json.Marshal(map[string]string{
		"session_id": sessionID,
		"cwd":        cwd,
		"prompt":     prompt,
	})
	if err != nil {
		t.Fatalf("marshal prompt hook input: %v", err)
	}
	ack, _ := json.Marshal(map[string]string{"id": sessionID})
	return runWindowsPromptFixture(t, string(input), map[string]string{
		"ENGRAM_PORT": port, "ENGRAM_TEST_ACK": string(ack),
		"ENGRAM_TEST_REGISTER_INPUT": filepath.Join(t.TempDir(), "registration"),
		"TMPDIR":                     filepath.ToSlash(os.TempDir()),
	})
}

func runWindowsPromptFixture(t *testing.T, input string, env map[string]string) string {
	t.Helper()
	pwsh := claudeCodePowerShell(t)
	dir := t.TempDir()
	child := filepath.Join(dir, "registration.ps1")
	wrapper := filepath.Join(dir, "wrapper.ps1")
	fake := `[IO.File]::WriteAllText($env:ENGRAM_TEST_REGISTER_INPUT, [Console]::In.ReadToEnd())
[IO.File]::WriteAllText($env:ENGRAM_TEST_REGISTER_INPUT + '.authority', "$env:ENGRAM_URL|$env:ENGRAM_PORT|$args")
if ($env:ENGRAM_TEST_REGISTER_FAIL -eq '1') { exit 1 }
[Console]::Write($env:ENGRAM_TEST_ACK)
`
	launch := `function engram {
  $data = @($input) -join ""
  $start = [Diagnostics.ProcessStartInfo]::new()
  $start.FileName = $env:ENGRAM_TEST_PWSH
  $start.Arguments = '-NoProfile -File "' + $env:ENGRAM_TEST_CHILD + '" ' + ($args -join ' ')
  $start.UseShellExecute = $false
  $start.RedirectStandardInput = $true
  $start.RedirectStandardOutput = $true
  $p = [Diagnostics.Process]::Start($start)
  $p.StandardInput.Write($data)
  $p.StandardInput.Close()
  $result = $p.StandardOutput.ReadToEnd()
  $p.WaitForExit()
  $global:LASTEXITCODE = $p.ExitCode
  $p.Dispose()
  $result
}
$before = $env:ENGRAM_URL
# Dot sourcing retains the real script's exit semantics; engine exit follows finally.
try { . $env:ENGRAM_TEST_ADAPTER } finally {
  if ($env:ENGRAM_URL -cne $before) { throw 'registration changed parent authority' }
}
`
	for path, text := range map[string]string{child: fake, wrapper: launch} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", wrapper)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := env[key]; !overridden && !strings.HasPrefix(strings.ToUpper(key), "ENGRAM_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range env {
		if key != "ENGRAM_URL" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "ENGRAM_URL=http://127.0.0.1:1", "TEMP="+env["TMPDIR"], "TMP="+env["TMPDIR"], "ENGRAM_TEST_CHILD="+child, "ENGRAM_TEST_PWSH="+pwsh, "ENGRAM_TEST_ADAPTER="+filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "user-prompt-submit.ps1"))
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell fixture: %v: %s", err, out)
	}
	return string(out)
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

func withoutEngramPort(environment []string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(strings.ToUpper(entry), "ENGRAM_PORT=") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
