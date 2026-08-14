package mcp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

type attributedWriteHarness struct {
	name       string
	handler    func(*store.Store, *SessionActivity) func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error)
	arguments  func(string) map[string]any
	target     func(*testing.T, *store.Store) string
	assertUses func(*testing.T, *SessionActivity, string, *mcppkg.CallToolResult)
}

func TestSessionAttributedWritesShareResolutionContract(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "engram")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	t.Chdir(projectDir)

	writes := sessionAttributedWriteHarnesses(t)
	cases := []struct {
		name       string
		explicitID string
		setup      func(*testing.T, storeCreator)
		wantID     string
		wantCode   string
		closeStore bool
	}{
		{name: "explicit known", explicitID: "known", setup: createActiveSessions("known"), wantID: "known"},
		{name: "explicit unknown", explicitID: "missing", setup: createEndedProjectMarker, wantCode: "unknown_session"},
		{name: "omitted with zero active runtime sessions", setup: createEndedProjectMarker, wantID: "manual-save-engram"},
		{name: "omitted with one active runtime session", setup: createActiveSessions("only"), wantID: "only"},
		{name: "omitted with multiple active runtime sessions", setup: createActiveSessions("first", "second"), wantCode: "ambiguous_session"},
		{name: "active session query error", wantCode: "session_resolution_failed", closeStore: true},
	}

	for _, write := range writes {
		for _, tc := range cases {
			t.Run(write.name+"/"+tc.name, func(t *testing.T) {
				s := newMCPTestStore(t)
				if tc.setup != nil {
					tc.setup(t, s)
				}
				before := attributedRowCounts(t, s)
				activity := NewSessionActivity(10 * time.Minute)
				if tc.closeStore {
					if err := s.Close(); err != nil {
						t.Fatalf("close store: %v", err)
					}
				}

				res, err := write.handler(s, activity)(context.Background(), mcppkg.CallToolRequest{
					Params: mcppkg.CallToolParams{Arguments: write.arguments(tc.explicitID)},
				})
				if err != nil {
					t.Fatalf("handler error: %v", err)
				}

				if tc.wantCode != "" {
					if !res.IsError {
						t.Fatalf("expected %s error, got success: %s", tc.wantCode, callResultText(t, res))
					}
					if !strings.Contains(callResultText(t, res), `"error_code":"`+tc.wantCode+`"`) {
						t.Fatalf("expected structured code %q, got %s", tc.wantCode, callResultText(t, res))
					}
					if !tc.closeStore {
						after := attributedRowCounts(t, s)
						if before != after {
							t.Fatalf("error path mutated rows: before=%v after=%v", before, after)
						}
					}
					activity.mu.Lock()
					activityEntries := len(activity.sessions)
					activity.mu.Unlock()
					if activityEntries != 0 {
						t.Fatalf("error path mutated activity for %d session(s)", activityEntries)
					}
					return
				}

				if res.IsError {
					t.Fatalf("unexpected error: %s", callResultText(t, res))
				}
				if got := write.target(t, s); got != tc.wantID {
					t.Fatalf("write attached to %q; want %q", got, tc.wantID)
				}
				write.assertUses(t, activity, tc.wantID, res)
			})
		}
	}
}

type storeCreator interface {
	CreateSession(id, project, directory string) error
	DB() *sql.DB
}

func createActiveSessions(ids ...string) func(*testing.T, storeCreator) {
	return func(t *testing.T, s storeCreator) {
		t.Helper()
		for _, id := range ids {
			if err := s.CreateSession(id, "engram", "/work/engram"); err != nil {
				t.Fatalf("create session %q: %v", id, err)
			}
		}
	}
}

func createEndedProjectMarker(t *testing.T, s storeCreator) {
	t.Helper()
	if err := s.CreateSession("ended-marker", "engram", "/work/engram"); err != nil {
		t.Fatalf("create ended marker: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE sessions SET ended_at = datetime('now') WHERE id = ?`, "ended-marker"); err != nil {
		t.Fatalf("end marker: %v", err)
	}
}

type attributedCounts struct {
	sessions     int
	observations int
	prompts      int
	mutations    int
}

func attributedRowCounts(t *testing.T, s interface{ DB() *sql.DB }) attributedCounts {
	t.Helper()
	var counts attributedCounts
	for _, item := range []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM sessions`, &counts.sessions},
		{`SELECT COUNT(*) FROM observations`, &counts.observations},
		{`SELECT COUNT(*) FROM user_prompts`, &counts.prompts},
		{`SELECT COUNT(*) FROM sync_mutations`, &counts.mutations},
	} {
		if err := s.DB().QueryRow(item.query).Scan(item.dest); err != nil {
			t.Fatalf("count rows: %v", err)
		}
	}
	return counts
}

func sessionAttributedWriteHarnesses(t *testing.T) []attributedWriteHarness {
	t.Helper()
	observationTarget := func(t *testing.T, s *store.Store) string {
		t.Helper()
		var id string
		if err := s.DB().QueryRow(`SELECT session_id FROM observations ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("query observation target: %v", err)
		}
		return id
	}
	promptTarget := func(t *testing.T, s *store.Store) string {
		t.Helper()
		var id string
		if err := s.DB().QueryRow(`SELECT session_id FROM user_prompts ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("query prompt target: %v", err)
		}
		return id
	}
	withSession := func(args map[string]any, id string) map[string]any {
		if id != "" {
			args["session_id"] = id
		}
		return args
	}

	return []attributedWriteHarness{
		{
			name: "mem_save",
			handler: func(s *store.Store, activity *SessionActivity) func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return handleSave(s, MCPConfig{DefaultProject: "engram"}, activity)
			},
			arguments: func(id string) map[string]any {
				return withSession(map[string]any{"title": "matrix", "content": "matrix memory", "type": "test"}, id)
			},
			target: observationTarget,
			assertUses: func(t *testing.T, activity *SessionActivity, id string, _ *mcppkg.CallToolResult) {
				activity.mu.Lock()
				defer activity.mu.Unlock()
				if state := activity.sessions[id]; state == nil || state.saveCount != 1 {
					t.Fatalf("save activity not recorded under final ID %q: %#v", id, state)
				}
			},
		},
		{
			name: "mem_save_prompt",
			handler: func(s *store.Store, activity *SessionActivity) func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return handleSavePrompt(s, MCPConfig{DefaultProject: "engram"}, activity)
			},
			arguments: func(id string) map[string]any {
				return withSession(map[string]any{"content": "matrix prompt"}, id)
			},
			target: promptTarget,
			assertUses: func(t *testing.T, activity *SessionActivity, id string, _ *mcppkg.CallToolResult) {
				if prompt, ok := activity.CurrentPrompt(id, "engram"); !ok || prompt != "matrix prompt" {
					t.Fatalf("prompt activity not recorded under final ID %q", id)
				}
			},
		},
		{
			name: "mem_session_summary",
			handler: func(s *store.Store, activity *SessionActivity) func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return handleSessionSummary(s, MCPConfig{DefaultProject: "engram"}, activity)
			},
			arguments: func(id string) map[string]any {
				return withSession(map[string]any{"content": "## Goal\nMatrix summary"}, id)
			},
			target: observationTarget,
			assertUses: func(t *testing.T, activity *SessionActivity, id string, res *mcppkg.CallToolResult) {
				activity.mu.Lock()
				_, wrong := activity.sessions[defaultSessionID("engram")]
				activity.mu.Unlock()
				if wrong && id != defaultSessionID("engram") {
					t.Fatalf("summary activity read created/used default ID instead of final ID %q: %s", id, callResultText(t, res))
				}
			},
		},
		{
			name: "mem_capture_passive",
			handler: func(s *store.Store, activity *SessionActivity) func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
				return handleCapturePassive(s, MCPConfig{DefaultProject: "engram"}, activity)
			},
			arguments: func(id string) map[string]any {
				return withSession(map[string]any{"content": "## Key Learnings:\n\n- Matrix passive learning is sufficiently descriptive"}, id)
			},
			target: observationTarget,
			assertUses: func(t *testing.T, activity *SessionActivity, id string, _ *mcppkg.CallToolResult) {
				activity.mu.Lock()
				defer activity.mu.Unlock()
				if state := activity.sessions[id]; state == nil || state.toolCallCount != 1 {
					t.Fatalf("passive activity not recorded under final ID %q: %#v", id, state)
				}
			},
		},
	}
}

func TestSessionStartAndEndUseAuthoritativeActivityID(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "engram")
	if err := os.Mkdir(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectDir)
	s := newMCPTestStore(t)
	activity := NewSessionActivity(time.Minute)

	start := handleSessionStart(s, MCPConfig{}, activity)
	res, err := start(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "runtime-id"}}})
	if err != nil || res.IsError {
		t.Fatalf("start failed: err=%v result=%s", err, callResultText(t, res))
	}
	activity.mu.Lock()
	_, runtimeTracked := activity.sessions["runtime-id"]
	_, manualTracked := activity.sessions[defaultSessionID("engram")]
	activity.mu.Unlock()
	if !runtimeTracked || manualTracked {
		t.Fatalf("start activity keys runtime=%v manual=%v", runtimeTracked, manualTracked)
	}

	end := handleSessionEnd(s, MCPConfig{}, activity)
	res, err = end(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "runtime-id", "summary": "first"}}})
	if err != nil || res.IsError {
		t.Fatalf("end failed: err=%v result=%s", err, callResultText(t, res))
	}
	activity.mu.Lock()
	_, runtimeTracked = activity.sessions["runtime-id"]
	activity.mu.Unlock()
	if runtimeTracked {
		t.Fatal("first end did not clear authoritative runtime activity")
	}

	activity.RecordToolCall("runtime-id")
	res, err = end(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "runtime-id", "summary": "replacement"}}})
	if err != nil || res.IsError {
		t.Fatalf("repeated end failed: err=%v result=%s", err, callResultText(t, res))
	}
	activity.mu.Lock()
	_, runtimeTracked = activity.sessions["runtime-id"]
	activity.mu.Unlock()
	if !runtimeTracked {
		t.Fatal("repeated end unexpectedly cleared activity")
	}

	res, err = end(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "unknown"}}})
	if err != nil {
		t.Fatalf("unknown end handler error: %v", err)
	}
	if !res.IsError || !strings.Contains(callResultText(t, res), "unknown_session") {
		t.Fatalf("unknown end should return structured unknown_session: %s", callResultText(t, res))
	}
}
