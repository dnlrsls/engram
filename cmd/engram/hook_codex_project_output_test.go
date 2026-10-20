package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Public lifecycle projection is opt-in. Selection/scope work must happen once
// in Go; returning its selected project must not repeat HTTP or inspection.
func TestCodexLifecycleProjectOutput(t *testing.T) {
	binary := buildCompatibilityBinary(t)
	for _, action := range []string{"codex-register", "codex-resolve"} {
		for _, scenario := range []string{"fresh", "owned-differing-client", "shared-continuation", "inspection-failure", "legacy-text", "legacy-json"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				root := t.TempDir()
				cwd := filepath.Join(root, "feature-x")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				owned := scenario == "owned-differing-client"
				shared := scenario == "shared-continuation"
				invalid := scenario == "inspection-failure"
				project := "myproj"
				configured := "myproj"
				if owned || shared {
					configured = "feature-x"
				}
				if shared {
					project = "feature-x"
				}
				hookProjectFixtureConfig(t, cwd, configured)
				if invalid {
					cwd = filepath.Join(root, "absent", "feature-x")
				}
				effective := "host"
				if shared {
					effective = "host:resume:2"
				}
				const ownerDir = "/persisted/myproj"
				type call struct {
					Method, URI string
					Body        map[string]any
				}
				var mu sync.Mutex
				var calls []call
				runtimePosts := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					var body map[string]any
					if len(raw) > 0 && json.Unmarshal(raw, &body) != nil {
						t.Errorf("invalid JSON body: %q", raw)
					}
					mu.Lock()
					calls = append(calls, call{r.Method, r.URL.RequestURI(), body})
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/sessions/host":
						if !owned && !shared {
							w.WriteHeader(404)
							io.WriteString(w, `{}`)
							return
						}
						row := map[string]any{"id": "host", "project": "myproj", "directory": ownerDir, "ownership_mode": "project_owned"}
						if shared {
							row["ownership_mode"] = "shared"
							row["ended_at"] = "2025-01-01T00:00:00Z"
						}
						json.NewEncoder(w).Encode(row)
					case "/health":
						io.WriteString(w, `{"status":"ok","service":"engram","capabilities":{"runtime_session_resolution":true}}`)
					case "/sessions":
						w.WriteHeader(201)
						ack := map[string]string{"id": effective, "status": "created"}
						if shared {
							ack["resumed_from"] = "host"
						}
						json.NewEncoder(w).Encode(ack)
					case "/runtime-sessions/resolve":
						runtimePosts++
						if shared && runtimePosts == 1 {
							w.WriteHeader(409)
							io.WriteString(w, `{"error":"runtime session scope conflict"}`)
							return
						}
						ack := map[string]string{"id": effective, "status": "resolved"}
						if shared {
							ack["resumed_from"] = "host"
						}
						json.NewEncoder(w).Encode(ack)
					default:
						t.Errorf("unexpected HTTP %s %s", r.Method, r.URL)
						w.WriteHeader(500)
					}
				}))
				defer server.Close()
				env := hookProjectFixtureEnvironment(t, root, server.URL)
				outputMode := "json-project"
				if scenario == "legacy-text" {
					outputMode = ""
				}
				if scenario == "legacy-json" {
					outputMode = "json"
				}
				if outputMode != "" {
					env = append(env, "ENGRAM_HOOK_OUTPUT="+outputMode)
				}
				input, err := json.Marshal(map[string]any{"session_id": "host", "cwd": cwd, "private": "never-echo-this", "tool_input": map[string]string{"project": "model-project", "session_id": "model-id"}})
				if err != nil {
					t.Fatal(err)
				}
				before := externalTree(t, root)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "hook", action)
				cmd.Dir, cmd.Env, cmd.Stdin = root, env, bytes.NewReader(input)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				runErr := cmd.Run()
				t.Logf("exit=%v stdout=%q stderr=%q", runErr, stdout.String(), stderr.String())
				if runErr != nil || stderr.Len() != 0 {
					t.Error("want exit0 and empty stderr")
				}
				wantOutput := ""
				if !invalid {
					switch outputMode {
					case "":
						wantOutput = effective + "\n"
					case "json":
						data, _ := json.Marshal(map[string]string{"session_id": effective})
						wantOutput = string(data) + "\n"
					default:
						data, _ := json.Marshal(map[string]string{"session_id": effective, "project": project})
						wantOutput = string(data) + "\n"
					}
				}
				if stdout.String() != wantOutput {
					t.Errorf("stdout=%q want exact %q", stdout.String(), wantOutput)
				}
				for _, secret := range []string{"never-echo-this", "model-project", "model-id"} {
					if strings.Contains(stdout.String(), secret) {
						t.Errorf("private input leaked: %q", secret)
					}
				}
				want := []call{{"GET", "/sessions/host", nil}}
				if !invalid {
					scope := map[string]any{"id": "host", "project": project, "directory": cwd}
					if action == "codex-register" {
						scope["resume"] = true
						want = append(want, call{"POST", "/sessions", scope})
					} else {
						want = append(want, call{"GET", "/health", nil}, call{"POST", "/runtime-sessions/resolve", scope})
						if shared {
							want = append(want, call{"GET", "/sessions/host", nil}, call{"POST", "/runtime-sessions/resolve", map[string]any{"id": "host", "project": project, "directory": ownerDir, "ownership_mode": "shared"}})
						}
					}
				}
				mu.Lock()
				actual := append([]call(nil), calls...)
				mu.Unlock()
				if !reflect.DeepEqual(actual, want) {
					t.Errorf("HTTP got=%#v want=%#v", actual, want)
				}
				if after := externalTree(t, root); !reflect.DeepEqual(after, before) {
					t.Errorf("complete fixture changed\nbefore=%v\nafter=%v", before, after)
				}
			})
		}
	}
}
