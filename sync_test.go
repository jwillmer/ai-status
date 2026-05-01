package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSupabase wires a single httptest.Server to handle both GoTrue
// (`/auth/v1/*`) and PostgREST (`/rest/v1/*`) requests, just like the
// real Supabase project layout. Tests configure response behaviour via
// the embedded fields.
type fakeSupabase struct {
	t *testing.T

	verifyAccessToken  string
	verifyRefreshToken string
	verifyExpiresIn    int

	pullRows  []syncRow
	pushedRow *syncRow

	rlsRows []map[string]any

	// 401-then-refresh probe: the first authed request returns 401,
	// the next refresh succeeds, the retried request returns 200.
	failFirstAuth atomic.Bool
}

func (f *fakeSupabase) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/auth/v1/otp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/auth/v1/verify", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{
			"access_token":  f.verifyAccessToken,
			"refresh_token": f.verifyRefreshToken,
			"expires_in":    f.verifyExpiresIn,
		})
	})
	mux.HandleFunc("/auth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{
			"access_token":  "refreshed-access",
			"refresh_token": f.verifyRefreshToken,
			"expires_in":    3600,
		})
	})
	mux.HandleFunc("/rest/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		// rlsProbe omits Authorization. Reflect the rlsRows fixture.
		if r.Header.Get("Authorization") == "" {
			writeJSONResp(w, f.rlsRows)
			return
		}
		if f.failFirstAuth.CompareAndSwap(true, false) {
			http.Error(w, "JWT expired", 401)
			return
		}
		switch r.Method {
		case "GET":
			writeJSONResp(w, f.pullRows)
		case "POST":
			body, _ := io.ReadAll(r.Body)
			var got []syncRow
			if err := json.Unmarshal(body, &got); err != nil {
				// PostgREST also accepts a single object; sync.go always
				// sends an object, so if [] decoding fails, try {}.
				var single syncRow
				if err2 := json.Unmarshal(body, &single); err2 != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				got = []syncRow{single}
			}
			if len(got) > 0 {
				echoed := got[0]
				if echoed.UpdatedAt.IsZero() {
					echoed.UpdatedAt = time.Now()
				}
				f.pushedRow = &echoed
				writeJSONResp(w, []syncRow{echoed})
				return
			}
			writeJSONResp(w, []syncRow{})
		default:
			http.Error(w, "method", 405)
		}
	})
	return mux
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeClient(t *testing.T, fake *fakeSupabase) *SyncClient {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "auth.json")
	return newSyncClient(syncConfig{
		SupabaseURL: srv.URL,
		AnonKey:     "anon",
	}, tokenFile)
}

func TestOTPVerifyHappyPath(t *testing.T) {
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
	}
	c := newFakeClient(t, fake)

	if err := c.otpStart(context.Background(), "u@example.com"); err != nil {
		t.Fatalf("otpStart: %v", err)
	}
	if err := c.otpVerify(context.Background(), "u@example.com", "123456"); err != nil {
		t.Fatalf("otpVerify: %v", err)
	}
	if !c.signedIn() {
		t.Error("signedIn should be true after verify")
	}
}

func TestPullSinceReturnsRows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
		pullRows: []syncRow{
			{ID: "a", Title: "A", RelPath: "a.md", CreatedAt: now, UpdatedAt: now, Body: "hi", BodyHash: hashBytes([]byte("hi"))},
		},
	}
	c := newFakeClient(t, fake)
	_ = c.otpVerify(context.Background(), "u@example.com", "123456")

	rows, err := c.pullSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "a" {
		t.Errorf("expected 1 row id=a, got %+v", rows)
	}
}

func TestPushOneRoundtrips(t *testing.T) {
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
	}
	c := newFakeClient(t, fake)
	_ = c.otpVerify(context.Background(), "u@example.com", "123456")

	row := syncRow{ID: "x", Title: "X", RelPath: "x.md", Body: "hi", BodyHash: hashBytes([]byte("hi"))}
	echoed, err := c.pushOne(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	if echoed == nil || echoed.ID != "x" {
		t.Errorf("expected echoed row id=x, got %+v", echoed)
	}
	if fake.pushedRow == nil {
		t.Error("server did not record a push")
	}
}

func TestRefreshOn401Retries(t *testing.T) {
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
		pullRows: []syncRow{
			{ID: "a", Title: "A", RelPath: "a.md", UpdatedAt: time.Now()},
		},
	}
	fake.failFirstAuth.Store(true)
	c := newFakeClient(t, fake)
	_ = c.otpVerify(context.Background(), "u@example.com", "123456")

	rows, err := c.pullSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull after refresh: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row after refresh, got %d", len(rows))
	}
}

func TestRLSProbeFailsWhenRowsLeak(t *testing.T) {
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
		rlsRows: []map[string]any{
			{"id": "leaked"},
		},
	}
	c := newFakeClient(t, fake)
	err := c.rlsProbe(context.Background())
	if err == nil {
		t.Fatal("rlsProbe should fail when anon read returns rows")
	}
	if !strings.Contains(err.Error(), "RLS") {
		t.Errorf("error should mention RLS, got %q", err.Error())
	}
}

func TestRLSProbeOKWhenEmpty(t *testing.T) {
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
		rlsRows: []map[string]any{},
	}
	c := newFakeClient(t, fake)
	if err := c.rlsProbe(context.Background()); err != nil {
		t.Fatalf("rlsProbe should pass when anon read returns []: %v", err)
	}
}

func TestSyncEngineFirstSyncDedupesByRelPath(t *testing.T) {
	store, root := newTestStore(t)
	// Local has a row with id "local-1" at plan.md.
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")
	_ = store.add(Session{ID: "local-1", Title: "Plan", Path: path, Created: time.Now()})

	now := time.Now().UTC().Truncate(time.Millisecond)
	fake := &fakeSupabase{
		t:                  t,
		verifyAccessToken:  "access-1",
		verifyRefreshToken: "refresh-1",
		verifyExpiresIn:    3600,
		// Cloud has the same RelPath but a different id.
		pullRows: []syncRow{
			{ID: "cloud-1", Title: "Plan", RelPath: "plan.md", CreatedAt: now, UpdatedAt: now, Body: "hello", BodyHash: hashBytes([]byte("hello"))},
		},
	}
	// Wire pullByRelPath to return the cloud row directly. Fake mux
	// already matches both — the rel_path query is the only GET path
	// the dedupe step issues.
	c := newFakeClient(t, fake)
	_ = c.otpVerify(context.Background(), "u@example.com", "123456")

	stateFile := filepath.Join(t.TempDir(), "state.json")
	state := loadSyncState(stateFile)
	supp := newSyncSuppressor()
	eng := newSyncEngine(c, store, state, supp, root, nil, nil)

	if err := eng.runSync(context.Background()); err != nil {
		t.Fatalf("runSync: %v", err)
	}

	// After dedupe + apply, the local row's id should be rewritten to
	// match cloud's "cloud-1" — there should not be both ids in the
	// store at this point, because the apply step then writes to the
	// new id.
	if _, hasCloud := store.byID("cloud-1"); !hasCloud {
		t.Error("expected store to contain cloud-1 after dedupe")
	}
}
