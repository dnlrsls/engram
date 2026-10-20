package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCodexLifecycleUnboundContext(t *testing.T) {
	binary := buildCompatibilityBinary(t)
	for _, action := range []string{"codex-register", "codex-resolve"} {
		for _, scenario := range []string{"missing-id", "empty-id", "numeric-id", "object-id", "ack-failure", "unusable-cwd", "owner-failure", "legacy-text", "legacy-json"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				cwd := filepath.Join(root, "feature-x")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				hookProjectFixtureConfig(t, cwd, "myproj")
				data := map[string]any{"cwd": cwd, "session_id": "host", "project": "model-top", "tool_input": map[string]string{"project": "model-project", "session_id": "model-session"}}
				mode := "json-project"
				projectOnly := true
				wantCalls, wantPosts := 0, 0
				switch scenario {
				case "missing-id":
					delete(data, "session_id")
				case "empty-id":
					data["session_id"] = ""
				case "numeric-id":
					data["session_id"] = 42
				case "object-id":
					data["session_id"] = map[string]string{"id": "invented"}
				default:
					wantCalls = 2
					wantPosts = 1
					if action == "codex-resolve" {
						wantCalls = 3
					}
				}
				if scenario == "unusable-cwd" {
					data["cwd"] = filepath.Join(root, "absent")
					projectOnly = false
					wantCalls = 1
					wantPosts = 0
				}
				if scenario == "owner-failure" {
					projectOnly = false
					wantCalls = 1
					wantPosts = 0
				}
				if scenario == "legacy-text" {
					mode = ""
					projectOnly = false
				}
				if scenario == "legacy-json" {
					mode = "json"
					projectOnly = false
				}
				var calls, posts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/sessions/host":
						if scenario == "owner-failure" {
							http.Error(w, "owner unavailable", 500)
						} else {
							http.NotFound(w, r)
						}
					case r.Method == http.MethodGet && r.URL.Path == "/health":
						_, _ = w.Write([]byte(`{"status":"ok","service":"engram","capabilities":{"runtime_session_resolution":true}}`))
					case r.Method == http.MethodPost && ((action == "codex-register" && r.URL.Path == "/sessions") || (action == "codex-resolve" && r.URL.Path == "/runtime-sessions/resolve")):
						posts.Add(1)
						var got map[string]any
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Error(err)
						}
						want := map[string]any{"id": "host", "directory": cwd, "project": "myproj"}
						if action == "codex-register" {
							want["resume"] = true
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("POST body=%#v want=%#v", got, want)
						}
						http.Error(w, "confirmation rejected", 503)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				for _, name := range []string{"home", "config", "data"} {
					if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				before := compatibilityTreeHash(t, root)
				input, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary, "hook", action)
				cmd.Env = append(os.Environ(), "ENGRAM_URL="+server.URL, "ENGRAM_SOCKET=", "ENGRAM_PROJECT=", "ENGRAM_HOOK_OUTPUT="+mode, "ENGRAM_DATA_DIR="+filepath.Join(root, "data"), "HOME="+filepath.Join(root, "home"), "USERPROFILE="+filepath.Join(root, "home"), "APPDATA="+filepath.Join(root, "config"), "LOCALAPPDATA="+filepath.Join(root, "config"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "ENGRAM_NO_UPDATE_CHECK=1")
				cmd.Stdin = bytes.NewReader(input)
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err = cmd.Run()
				want := ""
				if projectOnly {
					want = "{\"project\":\"myproj\",\"session_id\":\"\"}\n"
				}
				if err != nil || stdout.String() != want || stderr.Len() != 0 {
					t.Errorf("exit=%v stdout=%q stderr=%q want=%q", err, stdout.String(), stderr.String(), want)
				}
				if int(calls.Load()) != wantCalls || int(posts.Load()) != wantPosts {
					t.Errorf("HTTP calls/posts=%d/%d want=%d/%d", calls.Load(), posts.Load(), wantCalls, wantPosts)
				}
				if after := compatibilityTreeHash(t, root); after != before {
					t.Error("context-only result changed complete configured fixture tree")
				}
				if strings.Contains(stdout.String(), "model-") {
					t.Error("model identity/project selected output")
				}
			})
		}
	}
}
