package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// syncConfig is the persisted slice of Settings used by the sync layer.
// It carries no JWTs — those live in `data/sync-auth.json` so users can
// share a settings file without leaking credentials.
type syncConfig struct {
	Enabled         bool   `json:"enabled"`
	SupabaseURL     string `json:"supabaseUrl"`
	AnonKey         string `json:"anonKey"`
	AuthURLOverride string `json:"authUrlOverride,omitempty"`
	Email           string `json:"email,omitempty"`
}

// syncTokens persists the GoTrue access / refresh JWTs and their expiry.
// Stored in `data/sync-auth.json` with mode 0600 on Unix; Windows relies
// on user-profile ACLs (see docs/sync.md).
type syncTokens struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

// syncRow mirrors the public.sessions row in Supabase. JSON tags use
// snake_case to match the schema directly so PostgREST round-trips don't
// need a translation layer.
type syncRow struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	RelPath   string     `json:"rel_path"`
	Pinned    bool       `json:"pinned"`
	Archived  bool       `json:"archived"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at"`
	Body      string     `json:"body"`
	BodyHash  string     `json:"body_hash"`
}

// SyncClient wraps a single user's Supabase project: it owns the HTTP
// client, the on-disk token cache, and the small bit of in-memory state
// needed to refresh JWTs lazily on 401.
type SyncClient struct {
	mu        sync.Mutex
	cfg       syncConfig
	tokens    syncTokens
	tokenFile string
	http      *http.Client
}

// newSyncClient builds a client from persisted config + tokens. The
// returned client may be unauthenticated (empty tokens) — callers should
// check `signedIn()` before issuing data calls.
func newSyncClient(cfg syncConfig, tokenFile string) *SyncClient {
	c := &SyncClient{
		cfg:       cfg,
		tokenFile: tokenFile,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
	c.loadTokens()
	return c
}

func (c *SyncClient) loadTokens() {
	data, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &c.tokens)
}

func (c *SyncClient) saveTokensLocked() error {
	data, err := json.MarshalIndent(c.tokens, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.tokenFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.tokenFile)
}

// clearTokens wipes the on-disk and in-memory tokens. Used by
// /api/sync/signout. Leaves config untouched so the user can sign in
// again without re-pasting the URL + anon key.
func (c *SyncClient) clearTokens() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens = syncTokens{}
	if err := os.Remove(c.tokenFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *SyncClient) signedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens.AccessToken != ""
}

// authBase returns the GoTrue base URL — defaults to `${supabaseUrl}/auth/v1`
// (Supabase's standard layout) but is overridable for self-hosters who
// run GoTrue on a different origin.
func (c *SyncClient) authBase() string {
	if c.cfg.AuthURLOverride != "" {
		return strings.TrimRight(c.cfg.AuthURLOverride, "/")
	}
	return strings.TrimRight(c.cfg.SupabaseURL, "/") + "/auth/v1"
}

func (c *SyncClient) restBase() string {
	return strings.TrimRight(c.cfg.SupabaseURL, "/") + "/rest/v1"
}

// otpStart asks GoTrue to send a 6-digit code to `email`. The user
// pastes that code back via `otpVerify`. We use OTP rather than magic
// links because the latter requires a redirect URL allowlisted on the
// Supabase project — friction we'd otherwise impose on every user.
func (c *SyncClient) otpStart(ctx context.Context, email string) error {
	body, _ := json.Marshal(map[string]any{
		"email":       email,
		"create_user": true,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.authBase()+"/otp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	return nil
}

// otpVerify exchanges an email + code pair for an access + refresh JWT.
// On success the tokens are persisted and the client is "signed in"
// until the refresh token expires (Supabase default: 30 days, sliding).
func (c *SyncClient) otpVerify(ctx context.Context, email, token string) error {
	body, _ := json.Marshal(map[string]any{
		"email": email,
		"token": token,
		"type":  "email",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.authBase()+"/verify", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	var got struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return err
	}
	if got.AccessToken == "" {
		return fmt.Errorf("verify: empty access token")
	}
	c.mu.Lock()
	c.tokens = syncTokens{
		AccessToken:  got.AccessToken,
		RefreshToken: got.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(got.ExpiresIn) * time.Second),
	}
	err = c.saveTokensLocked()
	c.mu.Unlock()
	return err
}

// refresh swaps a refresh token for a new access token. Called lazily
// when a data call returns 401. One retry is enough — if the second
// call also fails, the user has to re-verify via OTP.
func (c *SyncClient) refresh(ctx context.Context) error {
	c.mu.Lock()
	rt := c.tokens.RefreshToken
	c.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("not signed in")
	}
	body, _ := json.Marshal(map[string]any{"refresh_token": rt})
	req, err := http.NewRequestWithContext(ctx, "POST", c.authBase()+"/token?grant_type=refresh_token", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	var got struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return err
	}
	c.mu.Lock()
	c.tokens.AccessToken = got.AccessToken
	if got.RefreshToken != "" {
		c.tokens.RefreshToken = got.RefreshToken
	}
	c.tokens.ExpiresAt = time.Now().Add(time.Duration(got.ExpiresIn) * time.Second)
	err = c.saveTokensLocked()
	c.mu.Unlock()
	return err
}

// doAuthed issues a request with apikey + Bearer headers and transparently
// retries once after refreshing the token on a 401. Callers get the raw
// response body (already read) so they can decode JSON without juggling
// closers.
func (c *SyncClient) doAuthed(ctx context.Context, req *http.Request) ([]byte, int, error) {
	c.mu.Lock()
	access := c.tokens.AccessToken
	c.mu.Unlock()
	if access == "" {
		return nil, 0, fmt.Errorf("not signed in")
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		return body, resp.StatusCode, nil
	}
	if err := c.refresh(ctx); err != nil {
		return body, 401, err
	}
	c.mu.Lock()
	access = c.tokens.AccessToken
	c.mu.Unlock()
	// Re-issue the request — http.Request bodies are single-use, so we
	// rebuild the body if there was one. Most calls are GET; for POST,
	// the caller passes a body that's safe to clone via GetBody (set
	// when we use bytes.NewReader, which net/http auto-wraps).
	req2 := req.Clone(ctx)
	req2.Header.Set("Authorization", "Bearer "+access)
	if req.GetBody != nil {
		b, err := req.GetBody()
		if err != nil {
			return nil, 0, err
		}
		req2.Body = b
	}
	resp2, err := c.http.Do(req2)
	if err != nil {
		return nil, 0, err
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	return body2, resp2.StatusCode, nil
}

// pullSince fetches every row updated after `since`, paginated 1000 at
// a time (PostgREST's default cap). Tombstones are included so deletes
// propagate. Rows come back sorted by updated_at ASC so the caller can
// advance the LastSyncAt cursor monotonically as it iterates.
func (c *SyncClient) pullSince(ctx context.Context, since time.Time) ([]syncRow, error) {
	out := []syncRow{}
	const pageSize = 1000
	offset := 0
	for {
		q := url.Values{}
		q.Set("select", "*")
		if !since.IsZero() {
			q.Set("updated_at", "gt."+since.UTC().Format(time.RFC3339Nano))
		}
		q.Set("order", "updated_at.asc")
		q.Set("limit", fmt.Sprintf("%d", pageSize))
		q.Set("offset", fmt.Sprintf("%d", offset))
		req, err := http.NewRequestWithContext(ctx, "GET", c.restBase()+"/sessions?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		body, status, err := c.doAuthed(ctx, req)
		if err != nil {
			return nil, err
		}
		if status >= 300 {
			return nil, fmt.Errorf("pull %d: %s", status, string(body))
		}
		var page []syncRow
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < pageSize {
			return out, nil
		}
		offset += pageSize
	}
}

// pullByID fetches a single row by ID. Used on session-open to refresh
// stale local copies without hitting the full pull path.
func (c *SyncClient) pullByID(ctx context.Context, id string) (*syncRow, error) {
	q := url.Values{}
	q.Set("select", "*")
	q.Set("id", "eq."+id)
	q.Set("limit", "1")
	req, err := http.NewRequestWithContext(ctx, "GET", c.restBase()+"/sessions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	body, status, err := c.doAuthed(ctx, req)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("pullByID %d: %s", status, string(body))
	}
	var rows []syncRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// pullByRelPath finds a live row by rel_path (case-sensitive, matching
// the partial unique index in the schema). Used by first-sync dedupe to
// pair a local file with an existing cloud row created on another
// device.
func (c *SyncClient) pullByRelPath(ctx context.Context, rel string) (*syncRow, error) {
	q := url.Values{}
	q.Set("select", "*")
	q.Set("rel_path", "eq."+rel)
	q.Set("deleted_at", "is.null")
	q.Set("limit", "1")
	req, err := http.NewRequestWithContext(ctx, "GET", c.restBase()+"/sessions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	body, status, err := c.doAuthed(ctx, req)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("pullByRelPath %d: %s", status, string(body))
	}
	var rows []syncRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// pushOne upserts a row. `Prefer: resolution=merge-duplicates` makes
// PostgREST treat the call as an upsert keyed by the primary key (id),
// and `return=representation` echoes the canonical row back so we pick
// up the server-stamped updated_at without a follow-up read.
func (c *SyncClient) pushOne(ctx context.Context, row syncRow) (*syncRow, error) {
	payload, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.restBase()+"/sessions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "resolution=merge-duplicates,return=representation")
	body, status, err := c.doAuthed(ctx, req)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("push %d: %s", status, string(body))
	}
	var got []syncRow
	if err := json.Unmarshal(body, &got); err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, nil
	}
	return &got[0], nil
}

// rlsProbe issues an unauthenticated GET against the sessions table to
// catch the two ways setup can go wrong: schema not applied (PostgREST
// returns 404 with a JSON object describing the missing relation), and
// RLS enabled-but-policies-missing or RLS off entirely (PostgREST returns
// 200 with a non-empty array because anon can read everything). A
// correctly-configured project responds 200 + [] (RLS denies the anon
// read silently). Called once at sign-in time so misconfiguration
// surfaces with a useful message instead of a confusing first-push
// error.
func (c *SyncClient) rlsProbe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.restBase()+"/sessions?select=id&limit=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.cfg.AnonKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return fmt.Errorf("sessions table not found — paste supabase/schema.sql into the Supabase SQL editor and run it, then sign in again")
	}
	if resp.StatusCode >= 300 && resp.StatusCode != 401 && resp.StatusCode != 403 {
		return fmt.Errorf("unexpected response from Supabase REST API (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		// Non-array response on a 200 means PostgREST returned an
		// error object — surface it rather than silently passing.
		var errObj map[string]any
		if json.Unmarshal(body, &errObj) == nil {
			if msg, _ := errObj["message"].(string); msg != "" {
				return fmt.Errorf("Supabase REST: %s", msg)
			}
		}
		return fmt.Errorf("unexpected response shape from Supabase REST: %s", strings.TrimSpace(string(body)))
	}
	if len(rows) > 0 {
		return fmt.Errorf("RLS appears disabled — anonymous read returned %d row(s); re-run supabase/schema.sql to enable policies", len(rows))
	}
	return nil
}

// apiError reads an error response body and formats a useful message.
// GoTrue and PostgREST both return JSON with `error_description` /
// `message` / `error` fields; we try them in turn before falling back
// to the raw body.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for _, k := range []string{"error_description", "msg", "message", "error"} {
		if v, ok := got[k].(string); ok && v != "" {
			return fmt.Errorf("%s: %s", resp.Status, v)
		}
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// sessionToRow converts the local store row into the wire format. Body
// is read fresh from disk so a stale BodyHash never ships.
func sessionToRow(s Session, sessRoot string) (syncRow, bool) {
	rel := s.RelPath
	if rel == "" {
		rel = relPathFor(s.Path, sessRoot)
	}
	if rel == "" {
		return syncRow{}, false
	}
	row := syncRow{
		ID:        s.ID,
		Title:     s.Title,
		RelPath:   rel,
		Pinned:    s.Pinned,
		Archived:  s.Archived,
		CreatedAt: s.Created,
		UpdatedAt: s.UpdatedAt,
		DeletedAt: s.DeletedAt,
		BodyHash:  s.BodyHash,
	}
	if s.DeletedAt == nil {
		body, err := os.ReadFile(s.Path)
		if err == nil {
			row.Body = string(body)
			if row.BodyHash == "" {
				row.BodyHash = hashBytes(body)
			}
		}
	}
	return row, true
}

// localPathFor resolves a row's RelPath against the configured sessions
// folder. Refuses paths containing `..` segments — sync writes only
// inside sessRoot.
func localPathFor(rel, sessRoot string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty rel_path")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == ".." {
			return "", fmt.Errorf("rel_path escapes sessions root: %s", rel)
		}
	}
	return filepath.Join(sessRoot, clean), nil
}

// SyncEngine is the orchestrator: it owns the client, the store, the
// suppressor, the persisted cursor, and the single mutex that
// serialises every sync trigger. All public entry points (app start,
// session open, fsnotify-driven push, manual /api/sync/now) funnel
// through `runSync` so two triggers can never apply changes
// concurrently.
type SyncEngine struct {
	mu           sync.Mutex
	client       *SyncClient
	store        *Store
	state        *SyncState
	suppressor   *SyncSuppressor
	sessRoot     string
	hub          *hub
	ensureWatch  func(dir string) // nil-tolerant; called when sync creates a new subdir
	lastErr      string
	lastAt       time.Time
}

func newSyncEngine(client *SyncClient, store *Store, state *SyncState, suppressor *SyncSuppressor, sessRoot string, h *hub, ensureWatch func(string)) *SyncEngine {
	return &SyncEngine{
		client:      client,
		store:       store,
		state:       state,
		suppressor:  suppressor,
		sessRoot:    sessRoot,
		hub:         h,
		ensureWatch: ensureWatch,
	}
}

// runSync is the only function that mutates both disk and cloud. It's
// idempotent and safe to call from any trigger — the engine mutex
// serialises overlapping calls.
//
// The dance, in order:
//  1. (First sync only) Walk the live cloud rowset and reconcile any
//     local row whose RelPath matches a cloud row with a different ID;
//     adopt the cloud ID. Keeps device-A and device-B from holding two
//     rows for the same file forever.
//  2. Pull rows updated since LastSyncAt. Apply each: tombstones delete
//     the local file; live rows write the body when newer than local.
//  3. Push every local row whose UpdatedAt is newer than LastSyncAt.
//  4. Advance LastSyncAt to the max updated_at observed in the pull
//     batch (or the local push batch's max, whichever is later).
func (e *SyncEngine) runSync(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.client.signedIn() {
		return fmt.Errorf("not signed in")
	}

	cursor := e.state.get()
	if cursor.IsZero() {
		if err := e.firstSyncDedupe(ctx); err != nil {
			e.recordErr(err)
			return err
		}
	}

	rows, err := e.client.pullSince(ctx, cursor)
	if err != nil {
		e.recordErr(err)
		return err
	}
	maxObserved := cursor
	for _, row := range rows {
		if err := e.applyRemote(row); err != nil {
			log.Printf("sync apply %s: %v", row.ID, err)
			continue
		}
		if row.UpdatedAt.After(maxObserved) {
			maxObserved = row.UpdatedAt
		}
	}

	// Push every local row whose UpdatedAt is newer than the cursor and
	// whose ID is not already in the pulled set (those just came down).
	pulled := make(map[string]bool, len(rows))
	for _, r := range rows {
		pulled[r.ID] = true
	}
	pushFailures := 0
	var firstPushErr error
	for _, sess := range e.store.listAll() {
		if pulled[sess.ID] {
			continue
		}
		if !sess.UpdatedAt.After(cursor) {
			continue
		}
		row, ok := sessionToRow(sess, e.sessRoot)
		if !ok {
			continue
		}
		echoed, err := e.client.pushOne(ctx, row)
		if err != nil {
			log.Printf("sync push %s: %v", sess.ID, err)
			pushFailures++
			if firstPushErr == nil {
				firstPushErr = err
			}
			continue
		}
		if echoed != nil && echoed.UpdatedAt.After(maxObserved) {
			maxObserved = echoed.UpdatedAt
		}
	}

	e.state.set(maxObserved)
	e.lastAt = time.Now()
	if pushFailures > 0 {
		// Surface a representative error so the status pill goes red
		// and the user has a hint why. Don't return it — partial
		// success should still advance the cursor and keep state from
		// stalling on a single bad row.
		e.lastErr = fmt.Sprintf("%d push(es) failed: %v", pushFailures, firstPushErr)
	} else {
		e.lastErr = ""
	}
	return nil
}

// firstSyncDedupe runs once (when the cursor is zero) and rewrites
// local IDs to match cloud IDs for any RelPath that already exists on
// the server. After this completes, the normal pull/push flow can rely
// on ID uniqueness end-to-end.
func (e *SyncEngine) firstSyncDedupe(ctx context.Context) error {
	for _, sess := range e.store.list() {
		rel := sess.RelPath
		if rel == "" {
			rel = relPathFor(sess.Path, e.sessRoot)
		}
		if rel == "" {
			continue
		}
		row, err := e.client.pullByRelPath(ctx, rel)
		if err != nil {
			return err
		}
		if row == nil || row.ID == sess.ID {
			continue
		}
		// Cloud has this RelPath under a different ID. Rewrite the
		// local row's ID to match the cloud's so they converge. We
		// keep the existing on-disk file untouched; the regular pull
		// path below will overwrite or push as appropriate based on
		// updated_at.
		_ = e.store.updateNoTouch(sess.ID, func(s *Session) {
			s.ID = row.ID
		})
	}
	return nil
}

// applyRemote applies one pulled row to the local state. Last-write-wins
// on UpdatedAt; equal-or-older remote rows are ignored.
func (e *SyncEngine) applyRemote(row syncRow) error {
	abs, err := localPathFor(row.RelPath, e.sessRoot)
	if err != nil {
		return err
	}
	local, hasLocal := e.store.byID(row.ID)
	tombstoned := row.DeletedAt != nil

	if tombstoned {
		// Cloud deletion. If our live row is older or absent, mirror
		// the delete locally; else our newer edit wins and will push
		// up in this same cycle.
		if !hasLocal {
			// Already gone or never existed locally — record the
			// tombstone so we don't re-push the deletion later.
			if t, ok := e.store.tombstoneByRelPath(row.RelPath); !ok || t.UpdatedAt.Before(row.UpdatedAt) {
				_ = e.upsertLocalTombstone(row)
			}
			return nil
		}
		if local.UpdatedAt.After(row.UpdatedAt) {
			return nil // local edit is newer, keep it
		}
		// Suppress fsnotify Remove echo, then delete the file + tombstone.
		e.suppressor.suppressRemove(local.Path)
		_ = os.Remove(local.Path)
		now := row.UpdatedAt
		_ = e.store.updateNoTouch(local.ID, func(s *Session) {
			s.DeletedAt = &now
			s.UpdatedAt = now
		})
		if e.hub != nil {
			e.hub.publishGlobal(buildGlobalEvent(local, local.Path))
		}
		return nil
	}

	if !hasLocal {
		// Tombstone for the same RelPath? Resurrect rather than
		// minting a fresh row — keeps the local store from
		// accumulating duplicate IDs when a deleted-then-recreated
		// file comes back through sync.
		if t, ok := e.store.tombstoneByRelPath(row.RelPath); ok && t.ID == row.ID {
			e.suppressor.suppressWrite(abs, row.BodyHash)
			if err := writeFileEnsureDir(abs, []byte(row.Body)); err != nil {
				return err
			}
			if e.ensureWatch != nil {
				e.ensureWatch(filepath.Dir(abs))
			}
			if err := e.store.resurrect(row.ID, abs, row.Body); err != nil {
				return err
			}
			_ = e.store.updateNoTouch(row.ID, func(s *Session) {
				s.Title = row.Title
				s.Pinned = row.Pinned
				s.Archived = row.Archived
				s.UpdatedAt = row.UpdatedAt
			})
			if e.hub != nil {
				updated, _ := e.store.byID(row.ID)
				e.hub.publishGlobal(buildGlobalEvent(updated, abs))
			}
			return nil
		}
		// New file from another device. Materialise it.
		return e.materialise(abs, row)
	}
	if !row.UpdatedAt.After(local.UpdatedAt) {
		return nil // ours is newer or equal
	}
	if local.BodyHash == row.BodyHash {
		// Metadata-only change (title/pin/archive). No file write.
		_ = e.store.updateNoTouch(local.ID, func(s *Session) {
			s.Title = row.Title
			s.Pinned = row.Pinned
			s.Archived = row.Archived
			s.UpdatedAt = row.UpdatedAt
			s.RelPath = row.RelPath
		})
		return nil
	}
	// Body change. Write the new body, suppressing the resulting
	// fsnotify Write so we don't push it back.
	e.suppressor.suppressWrite(abs, row.BodyHash)
	if err := writeFileEnsureDir(abs, []byte(row.Body)); err != nil {
		return err
	}
	_ = e.store.updateNoTouch(local.ID, func(s *Session) {
		s.Title = row.Title
		s.Pinned = row.Pinned
		s.Archived = row.Archived
		s.Path = abs
		s.RelPath = row.RelPath
		s.BodyHash = row.BodyHash
		s.UpdatedAt = row.UpdatedAt
	})
	if e.hub != nil {
		updated, _ := e.store.byID(local.ID)
		e.hub.publishGlobal(buildGlobalEvent(updated, abs))
	}
	return nil
}

// materialise creates a local file + store row for a row pulled from
// the cloud that we've never seen before. Mirrors registerDiscoveredFile
// but uses the cloud's ID and metadata directly so the rows converge.
func (e *SyncEngine) materialise(abs string, row syncRow) error {
	e.suppressor.suppressWrite(abs, row.BodyHash)
	if err := writeFileEnsureDir(abs, []byte(row.Body)); err != nil {
		return err
	}
	// If the row landed in a freshly-created subdir, hand it to the
	// watcher so the user's later edits trigger the SSE / push pipeline.
	if e.ensureWatch != nil {
		e.ensureWatch(filepath.Dir(abs))
	}
	sess := Session{
		ID:        row.ID,
		Title:     row.Title,
		Path:      abs,
		Created:   row.CreatedAt,
		Archived:  row.Archived,
		Pinned:    row.Pinned,
		RelPath:   row.RelPath,
		BodyHash:  row.BodyHash,
		UpdatedAt: row.UpdatedAt,
	}
	if err := e.store.add(sess); err != nil {
		return err
	}
	if e.hub != nil {
		e.hub.publishGlobal(buildGlobalEvent(sess, abs))
	}
	return nil
}

// upsertLocalTombstone records an inbound delete for a row we never had
// locally. Without this, a later pull-since-cursor would re-fetch and
// re-process the same tombstone forever.
func (e *SyncEngine) upsertLocalTombstone(row syncRow) error {
	now := row.UpdatedAt
	sess := Session{
		ID:        row.ID,
		Title:     row.Title,
		Path:      "",
		Created:   row.CreatedAt,
		RelPath:   row.RelPath,
		BodyHash:  row.BodyHash,
		UpdatedAt: now,
		DeletedAt: &now,
	}
	return e.store.add(sess)
}

func (e *SyncEngine) recordErr(err error) {
	e.lastErr = err.Error()
	e.lastAt = time.Now()
}

// status snapshots are returned by /api/sync/status. Read-only — no
// engine state is mutated. SupabaseURL / AnonKey / AuthURLOverride are
// public values (the anon key is intentionally exposed to clients), so
// shipping them lets the settings dialog pre-fill its fields without a
// separate read endpoint.
type syncStatus struct {
	Enabled         bool      `json:"enabled"`
	SignedIn        bool      `json:"signedIn"`
	Email           string    `json:"email"`
	SupabaseURL     string    `json:"supabaseUrl"`
	AnonKey         string    `json:"anonKey"`
	AuthURLOverride string    `json:"authUrlOverride,omitempty"`
	LastSyncAt      time.Time `json:"lastSyncAt"`
	LastError       string    `json:"lastError,omitempty"`
}

func (e *SyncEngine) status(cfg syncConfig) syncStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return syncStatus{
		Enabled:         cfg.Enabled,
		SignedIn:        e.client.signedIn(),
		Email:           cfg.Email,
		SupabaseURL:     cfg.SupabaseURL,
		AnonKey:         cfg.AnonKey,
		AuthURLOverride: cfg.AuthURLOverride,
		LastSyncAt:      e.lastAt,
		LastError:       e.lastErr,
	}
}

// writeFileEnsureDir is os.WriteFile with auto-mkdir of the parent. Used
// when materialising a session whose RelPath includes a subdirectory
// the user hasn't created locally yet (e.g. `archive/2026/plan.md`).
func writeFileEnsureDir(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
