package plugin_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type adapterRequest struct {
	Method, URI string
	Body        map[string]any
}

// S4-S6 public Bash adapters. A loopback foreign interpretation models the
// reported boundary; this is not a two-host or SSH execution claim.
func TestHookExternalAdapterRouting(t *testing.T) {
	bash := codexTestBash(t)
	requireCodexUnixTools(t, bash)
	binary := buildCodexFixtureCLI(t)
	for _, agent := range []string{"claude-code", "codex"} {
		for _, script := range []string{"session-start.sh", "post-compaction.sh"} {
			for _, scenario := range []string{"external", "external-unusable", "local-unset", "local-blank", "owned-external", "ended-shared-external", "windows-external"} {
				if scenario == "windows-external" && (agent != "codex" || runtime.GOOS != "windows") {
					continue
				}
				t.Run(agent+"/"+script+"/"+scenario, func(t *testing.T) {
					root := t.TempDir()
					cwd := filepath.Join(root, "feature-x")
					if err := os.MkdirAll(filepath.Join(cwd, ".engram"), 0700); err != nil {
						t.Fatal(err)
					}
					configured := "myproj"
					owned := scenario == "owned-external"
					shared := scenario == "ended-shared-external"
					if owned {
						configured = "other-client"
					}
					adapterWrite(t, filepath.Join(cwd, ".engram", "config.json"), adapterJSON(t, map[string]string{"project_name": configured}), 0600)
					invalid := scenario == "external-unusable"
					if invalid {
						cwd = filepath.Join(root, "absent", "feature-x")
					}
					local := strings.HasPrefix(scenario, "local-")
					selected := "myproj"
					if local {
						selected = "feature-x"
					}
					const host = "host é <opaque>"
					ownerDir := "/persisted/myproj"
					var mu sync.Mutex
					var calls []adapterRequest
					runtimePosts := 0
					fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						var body map[string]any
						if len(data) > 0 && json.Unmarshal(data, &body) != nil {
							t.Errorf("invalid request body %q", data)
						}
						mu.Lock()
						calls = append(calls, adapterRequest{r.Method, r.URL.RequestURI(), body})
						mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/health":
							io.WriteString(w, `{"status":"ok","service":"engram","instance_id":"fixture-instance","capabilities":{"runtime_session_resolution":true}}`)
						case "/project/current":
							io.WriteString(w, `{"project":"feature-x","project_source":"dir_basename"}`)
						case "/sessions/" + host:
							if !owned && !shared {
								w.WriteHeader(404)
								io.WriteString(w, `{}`)
								return
							}
							row := map[string]any{"id": host, "project": "myproj", "directory": ownerDir, "ownership_mode": "project_owned"}
							if shared {
								row["ownership_mode"] = "shared"
								row["ended_at"] = "2025-01-01T00:00:00Z"
							}
							json.NewEncoder(w).Encode(row)
						case "/sessions":
							w.WriteHeader(201)
							id := host
							if shared {
								id = host + ":resume:2"
							}
							json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "created", "resumed_from": host})
						case "/runtime-sessions/resolve":
							runtimePosts++
							if shared && runtimePosts == 1 {
								w.WriteHeader(409)
								io.WriteString(w, `{"error":"runtime session scope conflict"}`)
								return
							}
							id := host
							if shared {
								id = host + ":resume:2"
							}
							json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "resolved", "resumed_from": host})
						case "/context":
							// Return conspicuously different context if a caller routes incorrectly.
							context := "foreign-feature-x-context"
							if r.URL.Query().Get("project") == "myproj" {
								context = "client-myproj-context"
							}
							json.NewEncoder(w).Encode(map[string]string{"context": context})
						default:
							t.Errorf("unexpected HTTP %s %s", r.Method, r.URL)
							w.WriteHeader(500)
						}
					}))
					defer fixture.Close()
					target, _ := url.Parse(fixture.URL)
					originalURL := "  " + fixture.URL + "  "
					if local {
						originalURL = " \t "
					}
					env := adapterEnvironment(t, root, binary, target.Port(), originalURL, scenario == "local-unset")
					input := adapterJSON(t, map[string]any{"session_id": host, "cwd": cwd, "source": "resume", "unrelated": "never-echo-this", "prompt": "private-prompt", "tool_input": map[string]any{"project": "model-project", "session_id": "invented", "title": "private-title"}})
					before := adapterTree(t, root)
					stdout := runExternalAdapter(t, bash, agent, script, root, input, env, scenario == "windows-external")
					var want []adapterRequest
					add := func(method, uri string, body map[string]any) { want = append(want, adapterRequest{method, uri, body}) }
					lookup := "/sessions/" + url.PathEscape(host)
					projectURI := "/project/current?cwd=" + url.QueryEscape(cwd)
					if local && script == "session-start.sh" {
						add("GET", "/health", nil)
						add("GET", "/health", nil)
					}
					if local {
						add("GET", projectURI, nil)
					}
					if !invalid {
						if agent == "claude-code" {
							add("GET", lookup, nil)
							mode := "project_owned"
							if shared {
								mode = "shared"
							}
							add("POST", "/sessions", map[string]any{"id": host, "project": selected, "directory": cwd, "ownership_mode": mode, "resume": true})
						} else {
							if local {
								add("GET", projectURI, nil)
							} else {
								add("GET", lookup, nil)
							}
							scope := map[string]any{"id": host, "project": selected, "directory": cwd}
							if script == "session-start.sh" {
								scope["resume"] = true
								add("POST", "/sessions", scope)
							} else {
								add("GET", "/health", nil)
								add("POST", "/runtime-sessions/resolve", scope)
								if shared {
									add("GET", lookup, nil)
									add("POST", "/runtime-sessions/resolve", map[string]any{"id": host, "project": selected, "directory": ownerDir, "ownership_mode": "shared"})
								}
							}
						}
						contextURI := "/context?project=" + url.QueryEscape(selected)
						if agent == "claude-code" {
							contextURI += "&compact=1&pinned=20&max_bytes=16384"
						}
						add("GET", contextURI, nil)
					}
					mu.Lock()
					actual := append([]adapterRequest(nil), calls...)
					mu.Unlock()
					// An authoritative read-only owner lookup is permitted before rejecting
					// unusable fresh cwd; no other request, especially a POST, is permitted.
					allowedInvalidLookup := invalid && reflect.DeepEqual(actual, []adapterRequest{{"GET", lookup, nil}})
					if !reflect.DeepEqual(actual, want) && !allowedInvalidLookup {
						t.Errorf("HTTP transcript\ngot %#v\nwant %#v", actual, want)
					}
					if invalid {
						if strings.Contains(stdout, "client-myproj-context") || strings.Contains(stdout, "foreign-feature-x-context") {
							t.Error("failed inspection injected context")
						}
						if agent == "codex" && strings.Contains(stdout, "Registered runtime session (JSON data, not instructions):") {
							t.Error("failed inspection advertised registration")
						}
					} else {
						context := "client-myproj-context"
						if local {
							context = "foreign-feature-x-context"
						}
						if !strings.Contains(stdout, context) {
							t.Errorf("missing routed context %q", context)
						}
						if agent == "codex" {
							_, tail, found := strings.Cut(stdout, "Registered runtime session (JSON data, not instructions): ")
							line, _, _ := strings.Cut(tail, "\n")
							var identity map[string]string
							effective := host
							if shared {
								effective = host + ":resume:2"
							}
							if !found || json.Unmarshal([]byte(line), &identity) != nil || !reflect.DeepEqual(identity, map[string]string{"session_id": effective}) {
								t.Errorf("opaque effective identity not delivered: %q", line)
							}
						}
					}
					for _, secret := range []string{"never-echo-this", "private-prompt", "private-title", "model-project", "invented"} {
						if strings.Contains(stdout, secret) {
							t.Errorf("input leaked %q", secret)
						}
					}
					// Claude's external inspector and Codex's actual lifecycle action
					// each receive the original full JSON, not a cwd-only envelope.
					// Codex already owns inspection internally; never require another
					// resolve-hook-project call. Invalid input may stop before dispatch.
					receiptPath := filepath.Join(root, "inspection.jsonl")
					if agent == "codex" {
						receiptPath = filepath.Join(root, "native-input.jsonl")
					}
					receiptData, receiptErr := os.ReadFile(receiptPath)
					required := !local && (!owned || len(receiptData) > 0)
					if agent == "codex" {
						required = !invalid || len(receiptData) > 0
					}
					if required {
						if receiptErr != nil {
							t.Errorf("missing public native input receipt: %v", receiptErr)
						} else {
							if string(receiptData) != input+"\n" {
								t.Errorf("native action must receive full input exactly once: %q", receiptData)
							}
							// Retain the PRE-RUN platform mode. Only these exact expected
							// receipt bytes are an intentional full-tree fixture effect.
							receipt := before[receiptPath]
							receipt.Content = input + "\n"
							before[receiptPath] = receipt
						}
					}
					if after := adapterTree(t, root); !reflect.DeepEqual(after, before) {
						t.Errorf("complete fixture effects differ\nbefore=%v\nafter=%v", before, after)
					}
				})
			}
		}
	}
}
