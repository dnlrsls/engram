package plugin_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Canonical strings are independent fixtures, not the adapter's trim algorithm.
func TestClaudeBridgeOwnershipBash(t *testing.T) {
	requireHookBinaries(t)
	for _, test := range []struct {
		name, prompt, canonical, mutation, tools string
		registration, capture                    bool
	}{
		{name: "independent registration", prompt: "short", canonical: "short", registration: true},
		{name: "independent capture", prompt: "short", canonical: "short", capture: true},
		{name: "both", prompt: "original prompt", canonical: "original prompt", registration: true, capture: true},
		{name: "unset", prompt: "original prompt", canonical: "original prompt"},
		{name: "Pi alone", prompt: "original prompt", canonical: "original prompt", mutation: "pi"},
		{name: "malformed", prompt: "original prompt", mutation: "malformed"},
		{name: "array", prompt: "original prompt", mutation: "array"},
		{name: "null", prompt: "original prompt", mutation: "null"},
		{name: "wrong version", prompt: "original prompt", mutation: "version"},
		{name: "string version", prompt: "original prompt", mutation: "string version"},
		{name: "runtime type", prompt: "original prompt", mutation: "runtime type"},
		{name: "runtime blank", prompt: "original prompt", mutation: "runtime blank"},
		{name: "claude type", prompt: "original prompt", mutation: "claude type"},
		{name: "missing identity", prompt: "original prompt", mutation: "missing identity"},
		{name: "nested child", prompt: "original prompt", mutation: "child"},
		{name: "digest mismatch", prompt: "original prompt", canonical: "other prompt", registration: true, capture: false, mutation: "digest"},
		{name: "digest type", prompt: "original prompt", registration: true, mutation: "digest type"},
		{name: "uppercase digest", prompt: "original prompt", registration: true, mutation: "uppercase"},
		{name: "digest trailing newline", prompt: "original prompt", canonical: "original prompt", registration: true, mutation: "digest newline"},
		{name: "ECMAScript whitespace", prompt: "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufefforiginal prompt\ufeff\n", canonical: "original prompt", registration: true, capture: true},
		{name: "NEL retained", prompt: "\u0085original prompt\u0085", canonical: "\u0085original prompt\u0085", capture: true},
		{name: "zero width retained", prompt: "\u200boriginal prompt\u200b", canonical: "\u200boriginal prompt\u200b", capture: true},
		{name: "NEL not trimmed", prompt: "\u0085original prompt\u0085", canonical: "original prompt", registration: true, mutation: "digest"},
		{name: "zero width not trimmed", prompt: "\u200boriginal prompt\u200b", canonical: "original prompt", registration: true, mutation: "digest"},
		{name: "internal newline", prompt: "first\nsecond\n", canonical: "first\nsecond", capture: true},
		{name: "embedded NUL", prompt: "first\x00second\n", canonical: "first\x00second", capture: true},
		{name: "no hasher", prompt: "original prompt", canonical: "original prompt", registration: true, tools: "nohash"},
		{name: "failing hasher", prompt: "original prompt", canonical: "original prompt", registration: true, tools: "failhash"},
		{name: "partial parser failure", prompt: "original prompt", canonical: "original prompt", registration: true, tools: "failparser"},
		{name: "empty digest", prompt: "original prompt", registration: true, mutation: "missing digest"},
		{name: "blank claude identity", prompt: "original prompt", mutation: "claude blank"},
		{name: "no jq fallback", prompt: "original prompt", canonical: "original prompt", tools: "nojq"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			env := map[string]string{"HOME": root, "USERPROFILE": root, "TMPDIR": root, "TEMP": root, "TMP": root, "CLAUDE_CONFIG_DIR": root, "PI_CODING_AGENT_DIR": root, "PI_ENGRAM_BRIDGE_SESSION_REGISTER": "", "PI_ENGRAM_BRIDGE_PROMPT_CAPTURE": "", "PI_CODING_AGENT": "", "ENGRAM_DATA_DIR": root, "LC_ALL": "C.UTF-8", "LANG": "C.UTF-8"}
			sum := sha256.Sum256([]byte(test.canonical))
			marker := map[string]any{"version": 1, "runtimeSessionId": "pi-runtime", "claudeSessionId": "claude-parent", "promptDigest": hex.EncodeToString(sum[:])}
			switch test.mutation {
			case "version":
				marker["version"] = 2
			case "string version":
				marker["version"] = "1"
			case "runtime type":
				marker["runtimeSessionId"] = 42
			case "runtime blank":
				marker["runtimeSessionId"] = " \t"
			case "claude type":
				marker["claudeSessionId"] = 42
			case "claude blank":
				marker["claudeSessionId"] = " "
			case "missing digest":
				delete(marker, "promptDigest")
			case "missing identity":
				delete(marker, "runtimeSessionId")
			case "child":
				marker["claudeSessionId"] = "nested-child"
			case "digest type":
				marker["promptDigest"] = 42
			case "digest newline":
				marker["promptDigest"] = marker["promptDigest"].(string) + "\n"
			case "uppercase":
				marker["promptDigest"] = strings.ToUpper(marker["promptDigest"].(string))
			}
			encoded, _ := json.Marshal(marker)
			value := string(encoded)
			switch test.mutation {
			case "malformed":
				value = "{"
			case "array":
				value = "[" + value + "]"
			case "null":
				value = "null"
			case "pi":
				env["PI_CODING_AGENT"] = "1"
			}
			if test.registration || (test.mutation != "" && test.mutation != "pi") || test.tools != "" {
				env["PI_ENGRAM_BRIDGE_SESSION_REGISTER"] = value
			}
			if test.capture || (test.mutation != "" && test.mutation != "pi") || test.tools != "" {
				env["PI_ENGRAM_BRIDGE_PROMPT_CAPTURE"] = value
			}
			// A private PATH makes unsupported tools deterministic without touching installs.
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"cat", "dirname", "curl", "jq", "sha256sum"} {
				if (name == "jq" && test.tools == "nojq") || (name == "sha256sum" && test.tools == "nohash") {
					continue
				}
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatalf("required fixture tool %s: %v", name, err)
				}
				content := "#!/bin/bash\nexec '" + bashScriptPath(t, path) + "' \"$@\"\n"
				if name == "sha256sum" && test.tools == "failhash" {
					content = "#!/bin/bash\ncat >/dev/null\nprintf '" + hex.EncodeToString(sum[:]) + "  -\\n'\nexit 1\n"
				}
				if name == "jq" && test.tools == "failparser" {
					content = "#!/bin/bash\nif [[ \"$*\" == *-sj* ]]; then cat >/dev/null; printf 'original prompt'; exit 1; fi\nexec '" + bashScriptPath(t, path) + "' \"$@\"\n"
				}
				if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(bin, "engram"), []byte("#!/bin/bash\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			env["PATH"] = bashScriptPath(t, filepath.Join(bin, "engram"))
			env["PATH"] = strings.TrimSuffix(env["PATH"], "/engram")
			for _, key := range []string{"HOME", "TMPDIR", "CLAUDE_CONFIG_DIR", "ENGRAM_DATA_DIR"} {
				env[key] = strings.TrimSuffix(env["PATH"], "/bin")
			}
			var mu sync.Mutex
			writes := map[string][]map[string]any{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/project/current":
					io.WriteString(w, `{"project":"canonical-project","project_source":"config"}`)
				case "/context":
					io.WriteString(w, `{"context":"preserved memory context"}`)
				case "/sessions", "/prompts":
					if r.Method != http.MethodPost {
						t.Errorf("unexpected method %s", r.Method)
					}
					body := map[string]any{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					mu.Lock()
					writes[r.URL.Path] = append(writes[r.URL.Path], body)
					mu.Unlock()
					w.WriteHeader(201)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			env["ENGRAM_URL"] = srv.URL
			input, _ := json.Marshal(map[string]string{"session_id": "claude-parent", "cwd": root, "prompt": test.prompt})
			if test.tools != "nojq" {
				output := runHook(t, "session-start.sh", string(input), env)
				if !strings.Contains(output, "preserved memory context") || !strings.Contains(output, "ACTIVE PROTOCOL") {
					t.Fatalf("session output lost context/protocol: %s", output)
				}
			}
			expected := strings.TrimRight(strings.ReplaceAll(test.prompt, "\x00", ""), "\n")
			if test.name == "NEL not trimmed" || test.name == "zero width not trimmed" {
				// Preserve existing Unicode argv behavior, including native Windows jq.
				baselineEnv := make(map[string]string)
				for key, value := range env {
					baselineEnv[key] = value
				}
				baselineEnv["PI_ENGRAM_BRIDGE_PROMPT_CAPTURE"] = ""
				baselineInput, _ := json.Marshal(map[string]string{"session_id": "unicode-baseline", "cwd": root, "prompt": test.prompt})
				runHook(t, "user-prompt-submit.sh", string(baselineInput), baselineEnv)
				mu.Lock()
				if len(writes["/prompts"]) != 1 {
					mu.Unlock()
					t.Fatal("baseline must persist once")
				}
				expected = writes["/prompts"][0]["content"].(string)
				writes["/prompts"] = nil
				mu.Unlock()
			}
			output := decodeHookPayload(t, runHook(t, "user-prompt-submit.sh", string(input), env))
			assertToolSearchNames(t, selectNames(t, output.HookSpecificOutput.AdditionalContext))
			mu.Lock()
			defer mu.Unlock()
			wantSessions := 1
			if test.registration || test.tools == "nojq" {
				wantSessions = 0
			}
			wantPrompts := 1
			if test.capture {
				wantPrompts = 0
			}
			if len(writes["/sessions"]) != wantSessions || len(writes["/prompts"]) != wantPrompts {
				t.Fatalf("POST counts sessions=%d prompts=%d; want %d/%d", len(writes["/sessions"]), len(writes["/prompts"]), wantSessions, wantPrompts)
			}
			if wantSessions == 1 {
				body := writes["/sessions"][0]
				if body["id"] != "claude-parent" || body["project"] != "canonical-project" || body["directory"] != root || body["ownership_mode"] != "project_owned" {
					t.Fatalf("session body: %v", body)
				}
			}
			if wantPrompts == 1 {
				body := writes["/prompts"][0]
				if body["session_id"] != "claude-parent" || body["project"] != "canonical-project" || body["content"] != expected {
					t.Fatalf("prompt body: %v expected content %q", body, expected)
				}
			}
		})
	}
}
