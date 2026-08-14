package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionEndHTTPStrictAndIdempotentSideEffects(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("runtime", "engram", "/work/engram"); err != nil {
		t.Fatal(err)
	}
	srv := New(st, 0)
	writes := 0
	srv.SetOnWrite(func() { writes++ })

	unknown := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/sessions/unknown/end", strings.NewReader(`{"summary":"ignored"}`)))
	if unknown.Code != http.StatusNotFound || writes != 0 {
		t.Fatalf("unknown end status=%d writes=%d body=%s", unknown.Code, writes, unknown.Body.String())
	}

	first := httptest.NewRecorder()
	srv.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/sessions/runtime/end", strings.NewReader(`{"summary":"first"}`)))
	if first.Code != http.StatusOK || writes != 1 {
		t.Fatalf("first end status=%d writes=%d body=%s", first.Code, writes, first.Body.String())
	}
	ended, err := st.GetSession("runtime")
	if err != nil {
		t.Fatal(err)
	}

	repeated := httptest.NewRecorder()
	srv.Handler().ServeHTTP(repeated, httptest.NewRequest(http.MethodPost, "/sessions/runtime/end", strings.NewReader(`{"summary":"replacement"}`)))
	if repeated.Code != http.StatusOK || writes != 1 {
		t.Fatalf("repeated end status=%d writes=%d body=%s", repeated.Code, writes, repeated.Body.String())
	}
	after, err := st.GetSession("runtime")
	if err != nil {
		t.Fatal(err)
	}
	if *after.EndedAt != *ended.EndedAt || *after.Summary != *ended.Summary {
		t.Fatalf("repeated HTTP end changed session: before=%#v after=%#v", ended, after)
	}
}
