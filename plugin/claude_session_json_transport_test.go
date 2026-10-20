package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// Standalone S5-S7 regression: no dependency on the future identity-adapter
// matrix or its untracked helpers, and no native CLI build for read-only setup.
func TestClaudeSessionJSONTransport(t *testing.T) {
	bash := codexTestBash(t)
	for _, script := range []string{"session-start.sh", "post-compaction.sh"} {
		for _, tc := range []struct{ name, host string }{
			{"unicode", "host é <opaque>"},
			{"ascii-control", "host-ascii"},
		} {
			t.Run(script+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				cwd := filepath.Join(root, "work")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				type request struct {
					Method, URI string
					Body        map[string]any
				}
				var mu sync.Mutex
				var calls []request
				fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					var body map[string]any
					if len(raw) > 0 {
						if !utf8.Valid(raw) {
							t.Errorf("session JSON is not valid UTF-8: %x", raw)
						}
						if err := json.Unmarshal(raw, &body); err != nil {
							t.Errorf("decode session JSON: %v", err)
						}
					}
					mu.Lock()
					calls = append(calls, request{r.Method, r.URL.RequestURI(), body})
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/health":
						io.WriteString(w, `{"instance_id":"transport-fixture"}`)
					case "/project/current":
						io.WriteString(w, `{"project":"transport-project","project_source":"config"}`)
					case "/sessions/" + tc.host:
						w.WriteHeader(http.StatusNotFound)
					case "/sessions":
						if r.Header.Get("Content-Type") != "application/json" {
							t.Errorf("session Content-Type=%q", r.Header.Get("Content-Type"))
						}
						w.WriteHeader(http.StatusCreated)
					case "/context":
						io.WriteString(w, `{"context":"retained-transport-context"}`)
					default:
						t.Errorf("unexpected HTTP %s %s", r.Method, r.URL)
						w.WriteHeader(500)
					}
				}))
				defer fixture.Close()
				target, err := url.Parse(fixture.URL)
				if err != nil {
					t.Fatal(err)
				}
				env := claudeTransportEnvironment(t, root, target.Port())
				input, err := json.Marshal(map[string]any{"session_id": tc.host, "cwd": cwd, "source": "resume", "private": "must-not-echo", "tool_input": map[string]string{"session_id": "model-id", "project": "model-project"}})
				if err != nil {
					t.Fatal(err)
				}
				before := claudeTransportTree(t, root)
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, bash, filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", script))
				cmd.Dir, cmd.Env, cmd.Stdin = root, env, bytes.NewReader(input)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				runErr := cmd.Run()
				t.Logf("exit=%v stdout=%q stderr=%q", runErr, stdout.String(), stderr.String())
				if runErr != nil || stderr.Len() != 0 {
					t.Errorf("want exit0 and empty stderr")
				}
				wantOutput := "\nretained-transport-context\n"
				if script == "post-compaction.sh" {
					wantOutput = "CRITICAL INSTRUCTION POST-COMPACTION — follow these steps IN ORDER:\n" +
						"\n1. FIRST: Call mem_session_summary with the content of the compacted summary above. Use project: 'transport-project'.\n" +
						"   This preserves what was accomplished before compaction.\n\n" +
						"2. THEN: Call mem_context with project: 'transport-project' to recover recent session history and observations.\n" +
						"   Read the returned context carefully — it tells you what was being worked on.\n\n" +
						"3. If you need more detail on a specific topic, call mem_search with relevant keywords.\n\n" +
						"4. Only THEN continue working on what the user asked.\n\n" +
						"All 4 steps are MANDATORY. Without them, you lose context and start blind.\n\nretained-transport-context\n"
				}
				if stdout.String() != wantOutput {
					t.Errorf("stdout=%q want %q", stdout.String(), wantOutput)
				}
				for _, secret := range []string{"must-not-echo", "model-id", "model-project", tc.host} {
					if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
						t.Errorf("private/identity input leaked: %q", secret)
					}
				}
				var want []request
				if script == "session-start.sh" {
					want = append(want, request{"GET", "/health", nil}, request{"GET", "/health", nil})
				}
				want = append(want,
					request{"GET", "/project/current?cwd=" + url.QueryEscape(cwd), nil},
					request{"GET", "/sessions/" + url.PathEscape(tc.host), nil},
					request{"POST", "/sessions", map[string]any{"id": tc.host, "project": "transport-project", "directory": cwd, "ownership_mode": "project_owned", "resume": true}},
					request{"GET", "/context?project=transport-project&compact=1&pinned=20&max_bytes=16384", nil})
				mu.Lock()
				actual := append([]request(nil), calls...)
				mu.Unlock()
				if !reflect.DeepEqual(actual, want) {
					t.Errorf("HTTP got=%#v want=%#v", actual, want)
				}
				if after := claudeTransportTree(t, root); !reflect.DeepEqual(after, before) {
					t.Errorf("complete fixture changed\nbefore=%v\nafter=%v", before, after)
				}
			})
		}
	}
}

func claudeTransportEnvironment(t *testing.T, root, port string) []string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{bin, filepath.Join(root, "home"), filepath.Join(root, "config"), filepath.Join(root, "data"), filepath.Join(root, "tmp")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(filepath.ToSlash(s), "'", "'\"'\"'") + "'" }
	for _, tool := range []string{"cat", "dirname", "jq", "curl"} {
		executable, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		options := ""
		if tool == "curl" {
			options = " --disable --noproxy '*' --proxy '' --proto '=http' --globoff"
		}
		write(filepath.Join(bin, tool), "#!/bin/sh\nexec "+quote(executable)+options+" \"$@\"\n", 0700)
	}
	// Only setup/protocol reads are stubbed; unexpected native operations fail.
	write(filepath.Join(bin, "engram"), `#!/bin/sh
case "$1:$2" in
 instance-id:) printf '%s\n' transport-fixture ;;
 protocol-mode:claude-code) printf '%s\n' slim ;;
 *) printf '%s\n' 'unexpected transport fixture CLI action' >&2; exit 97 ;;
esac
`, 0700)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + filepath.Dir(git),
		"HOME=" + filepath.Join(root, "home"), "USERPROFILE=" + filepath.Join(root, "home"),
		"APPDATA=" + filepath.Join(root, "config"), "LOCALAPPDATA=" + filepath.Join(root, "config"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"ENGRAM_DATA_DIR=" + filepath.Join(root, "data"), "ENGRAM_PORT=" + port, "ENGRAM_URL=", "ENGRAM_SOCKET=",
		"TMPDIR=" + filepath.Join(root, "tmp"), "TMP=" + filepath.Join(root, "tmp"), "TEMP=" + filepath.Join(root, "tmp")}
	if runtime.GOOS == "windows" {
		startup := filepath.Join(root, "fixture-startup.sh")
		write(startup, "PATH=\"$PWD/bin:$PATH\"\nexport PATH\nunset BASH_ENV\n", 0600)
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"), "BASH_ENV="+filepath.ToSlash(startup))
	}
	return env
}

type claudeTransportEntry struct {
	Mode    os.FileMode
	Content string
}

// Includes every file/directory mode, every regular-file byte and symlink target.
func claudeTransportTree(t *testing.T, root string) map[string]claudeTransportEntry {
	t.Helper()
	result := map[string]claudeTransportEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content := ""
		if info.Mode()&os.ModeSymlink != 0 {
			content, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var b []byte
			b, err = os.ReadFile(path)
			content = string(b)
		}
		if err != nil {
			return err
		}
		result[path] = claudeTransportEntry{info.Mode(), content}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
