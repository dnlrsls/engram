package store

import (
	"errors"
	"testing"
)

func TestActiveRuntimeSessionsReturnsAllCandidatesWithoutRecencyOwnership(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"runtime-b", "manual-save-engram", "runtime-a", "ended"} {
		if err := s.CreateSession(id, "engram", "/work/engram"); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE sessions SET ended_at = datetime('now') WHERE id = ?`, "ended"); err != nil {
		t.Fatalf("end fixture: %v", err)
	}

	ids, err := s.ActiveRuntimeSessions("engram")
	if err != nil {
		t.Fatalf("ActiveRuntimeSessions: %v", err)
	}
	if len(ids) != 2 || ids[0] != "runtime-a" || ids[1] != "runtime-b" {
		t.Fatalf("active runtime candidates = %#v; want both runtime IDs in deterministic identity order", ids)
	}
}

func TestEndSessionIsStrictThenIdempotentWithoutRepeatedMutation(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("runtime", "engram", "/work/engram"); err != nil {
		t.Fatal(err)
	}

	before := countSessionSyncMutations(t, s, "runtime")
	changed, err := s.EndSession("runtime", "first summary")
	if err != nil || !changed {
		t.Fatalf("first EndSession changed=%v err=%v", changed, err)
	}
	first, err := s.GetSession("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if first.EndedAt == nil || first.Summary == nil || *first.Summary != "first summary" {
		t.Fatalf("unexpected first end state: %#v", first)
	}
	afterFirst := countSessionSyncMutations(t, s, "runtime")
	if afterFirst != before+1 {
		t.Fatalf("first end mutations=%d; want %d", afterFirst, before+1)
	}

	changed, err = s.EndSession("runtime", "replacement summary")
	if err != nil || changed {
		t.Fatalf("repeated EndSession changed=%v err=%v", changed, err)
	}
	second, err := s.GetSession("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if *second.EndedAt != *first.EndedAt || *second.Summary != *first.Summary {
		t.Fatalf("repeated end changed persisted state: first=%#v second=%#v", first, second)
	}
	if got := countSessionSyncMutations(t, s, "runtime"); got != afterFirst {
		t.Fatalf("repeated end enqueued mutation: got %d want %d", got, afterFirst)
	}
}

func TestEndSessionUnknownReturnsStableNotFoundWithoutMutation(t *testing.T) {
	s := newTestStore(t)
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	changed, err := s.EndSession("unknown", "ignored")
	if changed || !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("EndSession unknown changed=%v err=%v", changed, err)
	}
	var after int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unknown end mutated sync journal: before=%d after=%d", before, after)
	}
}

func countSessionSyncMutations(t *testing.T, s *Store, id string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sync_mutations WHERE entity = ? AND entity_key = ?`, SyncEntitySession, id).Scan(&count); err != nil {
		t.Fatalf("count session mutations: %v", err)
	}
	return count
}
