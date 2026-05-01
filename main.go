package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/atotto/clipboard"
	"github.com/fsnotify/fsnotify"
	"github.com/getlantern/systray"
	"github.com/pkg/browser"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	meta "github.com/yuin/goldmark-meta"
)

//go:embed static
var staticFS embed.FS

//go:embed skill/status-orchestrator/SKILL.md
var skillFile []byte

// Version and CommitSHA are stamped at build time via
// `-ldflags="-X main.Version=… -X main.CommitSHA=…"`. They power the update
// banner: when CommitSHA differs from origin/main on GitHub, the UI shows
// "N commits behind — Update" and offers an in-place git-pull + rebuild.
// Empty CommitSHA disables the update check (e.g. ad-hoc `go build` without
// the stamp).
var (
	Version   = "dev"
	CommitSHA = ""
)

// repoSlug is the GitHub repo polled for the latest commit on the default
// branch. Lowercase, no scheme, no trailing slash.
const repoSlug = "jwillmer/ai-status"

// skillPath is the absolute filesystem path where the embedded status-
// orchestrator SKILL.md is written at startup. Fresh `claude` invocations
// point at it so the agent loads the orchestrator role without requiring
// the user to have the skill pre-installed.
var skillPath string

// freshClaudePrompt builds the single-arg prompt passed to `claude` when
// starting a brand-new conversation for a session. It both loads the
// embedded status-orchestrator skill and names the status file, so the
// agent adopts the orchestrator role without requiring a skill install.
func freshClaudePrompt(statusPath string) string {
	if skillPath != "" {
		return "Read and follow " + skillPath + ", then use this for status: " + statusPath
	}
	return "Use this for status: " + statusPath
}

type Session struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Path     string    `json:"path"`
	Created  time.Time `json:"created"`
	Archived bool      `json:"archived"`
	Pinned   bool      `json:"pinned"`
	// Sync-related fields. RelPath is the path relative to the configured
	// sessions root; empty if the file lives outside it (project-local
	// session) — those rows are skipped by the sync engine. BodyHash is
	// sha256-hex of the last seen file body. UpdatedAt is bumped on any
	// metadata or body change. DeletedAt is a tombstone — non-nil rows are
	// hidden from list/byPath/byID and reaped after `tombstoneTTL`.
	RelPath   string     `json:"relPath,omitempty"`
	BodyHash  string     `json:"bodyHash,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt,omitempty"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
}

// tombstoneTTL is how long a soft-deleted session lingers in the store
// before it's permanently reaped. Long enough that an offline device
// coming back online can still observe and apply the deletion.
const tombstoneTTL = 30 * 24 * time.Hour

type Store struct {
	mu       sync.Mutex
	Sessions []Session `json:"sessions"`
	file     string
	sessRoot string // configured sessions folder, used to derive RelPath
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return err
	}
	return s.migrateLocked()
}

// migrateLocked backfills RelPath / BodyHash / UpdatedAt for any session
// missing them, and reaps tombstones older than tombstoneTTL. Idempotent
// on subsequent loads.
func (s *Store) migrateLocked() error {
	dirty := false
	now := time.Now()
	kept := s.Sessions[:0]
	for i := range s.Sessions {
		sess := s.Sessions[i]
		if sess.DeletedAt != nil && now.Sub(*sess.DeletedAt) > tombstoneTTL {
			dirty = true
			continue
		}
		if sess.RelPath == "" {
			if rp := relPathFor(sess.Path, s.sessRoot); rp != "" {
				sess.RelPath = rp
				dirty = true
			}
		}
		if sess.UpdatedAt.IsZero() {
			sess.UpdatedAt = fileModTime(sess.Path)
			if sess.UpdatedAt.IsZero() {
				sess.UpdatedAt = sess.Created
			}
			dirty = true
		}
		if sess.BodyHash == "" && sess.DeletedAt == nil {
			if h, ok := hashFile(sess.Path); ok {
				sess.BodyHash = h
				dirty = true
			}
		}
		kept = append(kept, sess)
	}
	s.Sessions = kept
	if dirty {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

func (s *Store) add(sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.RelPath == "" {
		sess.RelPath = relPathFor(sess.Path, s.sessRoot)
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = time.Now()
	}
	if sess.BodyHash == "" {
		if h, ok := hashFile(sess.Path); ok {
			sess.BodyHash = h
		}
	}
	s.Sessions = append(s.Sessions, sess)
	return s.saveLocked()
}

func (s *Store) update(id string, fn func(*Session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Sessions {
		if s.Sessions[i].ID == id && s.Sessions[i].DeletedAt == nil {
			fn(&s.Sessions[i])
			s.Sessions[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("not found: %s", id)
}

// updateNoTouch mutates a session without bumping UpdatedAt — used by the
// sync engine when applying remote rows so we don't shadow the remote
// timestamp with a local "now" that would then race back as a push.
func (s *Store) updateNoTouch(id string, fn func(*Session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Sessions {
		if s.Sessions[i].ID == id {
			fn(&s.Sessions[i])
			return s.saveLocked()
		}
	}
	return fmt.Errorf("not found: %s", id)
}

// remove is a soft delete: marks the row as a tombstone and bumps
// UpdatedAt. The on-disk file is removed by the caller (it predates the
// store and may live outside sessRoot). Tombstones are hidden from
// list/byPath/byID and reaped after tombstoneTTL on next load.
func (s *Store) remove(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Sessions {
		if s.Sessions[i].ID == id && s.Sessions[i].DeletedAt == nil {
			now := time.Now()
			s.Sessions[i].DeletedAt = &now
			s.Sessions[i].UpdatedAt = now
			out := s.Sessions[i]
			return out, s.saveLocked()
		}
	}
	return Session{}, fmt.Errorf("not found: %s", id)
}

// resurrect undoes a soft delete and is used when a file reappears at the
// same RelPath as an existing tombstone. Cheaper than minting a new ID
// and prevents drift between devices that may still be holding the row.
func (s *Store) resurrect(id string, path, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Sessions {
		if s.Sessions[i].ID == id {
			s.Sessions[i].DeletedAt = nil
			s.Sessions[i].Path = path
			s.Sessions[i].RelPath = relPathFor(path, s.sessRoot)
			s.Sessions[i].BodyHash = hashBytes([]byte(body))
			s.Sessions[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return fmt.Errorf("not found: %s", id)
}

func (s *Store) list() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.Sessions))
	for _, sess := range s.Sessions {
		if sess.DeletedAt != nil {
			continue
		}
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// listAll returns every session including tombstones — used by the sync
// engine to push deletes upstream.
func (s *Store) listAll() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, len(s.Sessions))
	copy(out, s.Sessions)
	return out
}

func (s *Store) byPath(p string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.Sessions {
		if sess.DeletedAt != nil {
			continue
		}
		if strings.EqualFold(sess.Path, p) {
			return sess, true
		}
	}
	return Session{}, false
}

// tombstoneByRelPath returns the most recent tombstoned session whose
// RelPath matches (case-insensitive). Used to resurrect an entry when a
// file reappears at the same relative path.
func (s *Store) tombstoneByRelPath(rel string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rel == "" {
		return Session{}, false
	}
	var best Session
	found := false
	for _, sess := range s.Sessions {
		if sess.DeletedAt == nil {
			continue
		}
		if !strings.EqualFold(sess.RelPath, rel) {
			continue
		}
		if !found || sess.UpdatedAt.After(best.UpdatedAt) {
			best = sess
			found = true
		}
	}
	return best, found
}

func (s *Store) byID(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.Sessions {
		if sess.ID == id && sess.DeletedAt == nil {
			return sess, true
		}
	}
	return Session{}, false
}

// relPathFor returns the slash-separated path of `abs` relative to
// `sessRoot`, or "" if abs lives outside sessRoot. Sync skips empty
// RelPath rows so project-local sessions stay device-local.
func relPathFor(abs, sessRoot string) string {
	if abs == "" || sessRoot == "" {
		return ""
	}
	rel, err := filepath.Rel(sessRoot, abs)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// pub/sub: per-session (full payload) + global (cross-session notify stream)
type hub struct {
	mu        sync.Mutex
	subs      map[string]map[chan string]struct{}
	globalCh  map[chan string]struct{}
}

func newHub() *hub {
	return &hub{
		subs:     map[string]map[chan string]struct{}{},
		globalCh: map[chan string]struct{}{},
	}
}

func (h *hub) subscribe(id string) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 4)
	if h.subs[id] == nil {
		h.subs[id] = map[chan string]struct{}{}
	}
	h.subs[id][ch] = struct{}{}
	return ch
}

func (h *hub) unsubscribe(id string, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.subs[id]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.subs, id)
		}
	}
	close(ch)
}

func (h *hub) publish(id, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[id] {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *hub) subscribeGlobal() chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 16)
	h.globalCh[ch] = struct{}{}
	return ch
}

func (h *hub) unsubscribeGlobal(ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.globalCh, ch)
	close(ch)
}

func (h *hub) publishGlobal(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.globalCh {
		select {
		case ch <- msg:
		default:
		}
	}
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9\-_]+`)

func slug(s string) string {
	s = strings.TrimSpace(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "session"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Typographer, meta.Meta, &wikilinkExt{}),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

func renderMD(path string) (string, error) {
	out, _, err := renderMDWithSource(path)
	return out, err
}

// renderMDWithSource renders the markdown file and also returns the raw
// source, so callers can diff it against a cached previous version.
func renderMDWithSource(path string) (string, string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var buf strings.Builder
	if err := md.Convert(src, &buf); err != nil {
		return "", "", err
	}
	return buf.String(), string(src), nil
}

func fileModTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// hashBytes returns the lowercase hex sha256 digest of b. Used as the
// canonical change signal for a session body — cheap, deterministic, and
// unaffected by mtime drift across filesystems and clones.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashFile reads `path` and returns its sha256 hex digest plus a success
// flag. Missing or unreadable files return ("", false) — callers leave
// the existing hash untouched in that case.
func hashFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return hashBytes(data), true
}

// buildUpdate returns a JSON string with rendered HTML + file mtime. When
// prev has a cached previous version for sessionID, new-lines are marked
// in the HTML output and hasDiff is set. The cache is updated with the
// current source so the next call diffs against it.
func buildUpdate(path, sessionID string, prev *prevCache) string {
	htmlOut, src, err := renderMDWithSource(path)
	if err != nil {
		return ""
	}
	_, _, focus, _ := parseSessionMeta(path)
	payload := map[string]any{
		"updated": fileModTime(path),
		"focus":   focus,
	}
	if prev != nil && sessionID != "" {
		if old, ok := prev.get(sessionID); ok {
			if set := newLines(old, src); len(set) > 0 {
				htmlOut = markHTML(htmlOut, set)
				payload["hasDiff"] = true
			}
		}
		prev.set(sessionID, src)
	}
	payload["html"] = htmlOut
	b, _ := json.Marshal(payload)
	return string(b)
}

var tagRe = regexp.MustCompile(`<[^>]+>`)
var wsRe = regexp.MustCompile(`\s+`)

func plainSnippet(htmlSrc string, max int) string {
	txt := tagRe.ReplaceAllString(htmlSrc, " ")
	txt = wsRe.ReplaceAllString(txt, " ")
	txt = strings.TrimSpace(txt)
	if len([]rune(txt)) > max {
		r := []rune(txt)
		txt = string(r[:max]) + "…"
	}
	return txt
}

// buildGlobalEvent returns a JSON string for the cross-session notify stream.
// `path` is included so external moves (skill or another tab) can refresh the
// path shown in any tab that already has the session open — without needing
// a full reload to pick up the new location.
func buildGlobalEvent(sess Session, path string) string {
	htmlOut, _ := renderMD(path)
	b, _ := json.Marshal(map[string]any{
		"sessionId": sess.ID,
		"title":     titleFor(sess),
		"path":      path,
		"updated":   fileModTime(path),
		"snippet":   plainSnippet(htmlOut, 140),
	})
	return string(b)
}

func sessionWithUpdated(s Session) map[string]any {
	folder, claudeSession, focus, _ := parseSessionMeta(s.Path)
	return map[string]any{
		"id":            s.ID,
		"title":         titleFor(s),
		"path":          s.Path,
		"created":       s.Created,
		"archived":      s.Archived,
		"pinned":        s.Pinned,
		"updated":       fileModTime(s.Path),
		"folder":        folder,
		"claudeSession": claudeSession,
		"focus":         focus,
	}
}

// fileTitle returns the first `# heading` line in the given markdown file,
// or "" if none is found within the first few kilobytes. YAML front matter
// (`---`-fenced block at the top) is skipped so fields like `title:` inside
// it cannot false-match.
var titleLineRe = regexp.MustCompile(`(?m)^\s*title:\s*(.*?)\s*$`)

func fileTitle(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > 8192 {
		data = data[:8192]
	}
	s := string(data)
	// Prefer YAML `title:` — skill's canonical source of truth now.
	if fm, rest, ok := splitFrontMatter(s); ok {
		if m := titleLineRe.FindStringSubmatch(fm); len(m) == 2 {
			t := strings.TrimSpace(m[1])
			// Strip surrounding quotes (single or double).
			t = strings.Trim(t, "'\"")
			if t != "" {
				return t
			}
		}
		s = rest
	}
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}

// titleFor resolves the display title for a session: the file's H1 wins,
// falling back to the filename stem. The store's Title field is no longer
// authoritative — this makes manual edits to the `.md` file show up in the UI.
func titleFor(s Session) string {
	if t := fileTitle(s.Path); t != "" {
		return t
	}
	base := filepath.Base(s.Path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// setFileTitle writes the new title into the YAML front matter's `title:`
// field (creating the field or the whole front matter block if missing).
// Backward compat: if the file has no front matter but carries a legacy
// H1, the H1 is updated in place instead.
func setFileTitle(path, newTitle string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	quoted := yamlQuote(newTitle)

	// Front-matter path.
	if fm, rest, ok := splitFrontMatter(text); ok {
		newFm := ""
		if titleLineRe.MatchString(fm) {
			newFm = titleLineRe.ReplaceAllString(fm, "title: "+quoted)
		} else {
			// Prepend title: to the existing front matter.
			newFm = "title: " + quoted + "\n" + fm
		}
		rebuilt := "---\n" + newFm
		if !strings.HasSuffix(newFm, "\n") {
			rebuilt += "\n"
		}
		rebuilt += "---\n" + rest
		return os.WriteFile(path, []byte(rebuilt), 0644)
	}

	// Legacy (no front matter): edit the H1 in place or prepend one.
	lines := strings.Split(text, "\n")
	replaced := false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "# ") {
			lines[i] = "# " + newTitle
			replaced = true
			break
		}
		if i > 50 {
			break
		}
	}
	if !replaced {
		lines = append([]string{"# " + newTitle, ""}, lines...)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

var appURL string
var termManager = newTerminalManager()

func main() {
	addr := flag.String("addr", "127.0.0.1:7879", "listen address")
	root := flag.String("root", ".", "data root")
	noTray := flag.Bool("no-tray", false, "run without system tray icon")
	noOpen := flag.Bool("no-open", false, "do not auto-open browser on start")
	flag.Parse()

	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	appURL = "http://" + *addr

	// Windows GUI builds (-H windowsgui) have no stdout. Redirect logs to a file
	// inside the data root so we don't silently swallow errors.
	if lf, err := os.OpenFile(filepath.Join(rootAbs, "status-updates.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		log.SetOutput(lf)
	}

	// Drop any leftover binary from a prior self-update. Windows-only
	// in practice — see cleanupStaleBinary in update_windows.go.
	if exe, _, err := resolvedExe(); err == nil {
		cleanupStaleBinary(exe)
	}

	ln, err := listenWithUpdateGrace(*addr)
	if err != nil {
		// Port busy → assume another instance is already running.
		// Open browser tab and exit quietly.
		log.Printf("listen %s failed (%v); assuming another instance is running, opening browser", *addr, err)
		if !*noOpen {
			_ = browser.OpenURL(appURL)
		}
		return
	}

	srvErr := make(chan error, 1)
	go func() { srvErr <- runServer(ln, rootAbs) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		termManager.KillAll()
		os.Exit(0)
	}()

	if !*noOpen {
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = browser.OpenURL(appURL)
		}()
	}

	if *noTray {
		if err := <-srvErr; err != nil {
			termManager.KillAll()
			log.Fatal(err)
		}
		return
	}

	onReady := func() {
		sub, _ := fs.Sub(staticFS, "static")
		systray.SetIcon(trayIconBytes(sub))
		systray.SetTitle("")
		systray.SetTooltip("AI Status — " + appURL)

		openItem := systray.AddMenuItem("Open in browser", "Open AI Status")
		copyItem := systray.AddMenuItem("Copy URL", "Copy URL to clipboard")
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit", "Quit AI Status")

		go func() {
			for {
				select {
				case <-openItem.ClickedCh:
					_ = browser.OpenURL(appURL)
				case <-copyItem.ClickedCh:
					_ = clipboard.WriteAll(appURL)
				case <-quitItem.ClickedCh:
					termManager.KillAll()
					systray.Quit()
					// systray.Run() unwind is unreliable on Linux/AppIndicator;
					// guarantee process exit so Quit always works.
					os.Exit(0)
				case err := <-srvErr:
					if err != nil {
						log.Println("server:", err)
					}
					termManager.KillAll()
					systray.Quit()
					os.Exit(0)
				}
			}
		}()
	}
	systray.Run(onReady, func() {})
}

func runServer(ln net.Listener, rootAbs string) error {
	dataDir := filepath.Join(rootAbs, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	settings := loadSettings(rootAbs, filepath.Join(dataDir, "settings.json"))
	sessDir := settings.resolvedSessionsFolder()
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		return err
	}
	log.Printf("sessions folder: %s", sessDir)

	store := &Store{file: filepath.Join(dataDir, "sessions.json"), sessRoot: sessDir}
	if err := store.load(); err != nil {
		return err
	}
	if added := discoverSessions(sessDir, store); added > 0 {
		log.Printf("auto-discovered %d session file(s) in %s", added, sessDir)
	}

	// Write the embedded SKILL.md to disk so `claude` can read it by path
	// without requiring the user to have the skill pre-installed. Overwrite
	// on every start so binary upgrades always refresh the on-disk copy.
	skillPath = filepath.Join(dataDir, "status-orchestrator.SKILL.md")
	if err := os.WriteFile(skillPath, skillFile, 0644); err != nil {
		log.Printf("skill write: %v", err)
	}

	// Warm the version cache in the background so the first /api/version
	// call from the browser is served from cache instead of waiting on
	// GitHub. Non-blocking: a slow or failed lookup never delays startup
	// or the listener.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		info := fetchUpdateInfo(ctx)
		if info.UpdateAvailable {
			log.Printf("update available: %s → %s (%d behind)", info.Current, info.Latest, info.Behind)
		}
	}()

	h := newHub()
	prev := newPrevCache()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dw := newDirWatcher(w)
	// sessDir is always watched (auto-discovery target). Refcount it once
	// up front so it never gets removed even if no session lives there.
	if err := dw.ensure(sessDir); err != nil {
		return err
	}
	// Walk sessDir and watch every subdirectory too — sync may
	// materialise files into nested paths (`archive/2026/plan.md`),
	// and without a watch on the parent dir, fsnotify won't see the
	// user's later edits.
	watchSubdirs(sessDir, dw)

	syncStateStore := loadSyncState(filepath.Join(dataDir, "sync-state.json"))
	ensureWatch := func(dir string) {
		if dir == "" || dir == sessDir {
			return
		}
		if err := dw.ensure(dir); err != nil {
			log.Printf("watcher add %s: %v", dir, err)
		}
	}
	// atomic.Pointer wrappers because /api/sync/config rebuilds both on
	// settings change while the debounced-push goroutine and other
	// handlers may be reading them concurrently. Plain var rebind would
	// be a data race under -race.
	var syncClientHolder atomic.Pointer[SyncClient]
	var syncEngineHolder atomic.Pointer[SyncEngine]
	syncClientHolder.Store(newSyncClient(settings.syncCfg(), filepath.Join(dataDir, "sync-auth.json")))
	syncEngineHolder.Store(newSyncEngine(syncClientHolder.Load(), store, syncStateStore, syncSuppressor, sessDir, h, ensureWatch))

	// First sync at startup, in the background so a slow Supabase round
	// trip never delays the listener. Disabled config skips the call.
	go func() {
		if !settings.syncCfg().Enabled || !syncClientHolder.Load().signedIn() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := syncEngineHolder.Load().runSync(ctx); err != nil {
			log.Printf("startup sync: %v", err)
		}
	}()

	// Push debounced local edits up to the cloud. Subscribed to the
	// global hub so every fsnotify-driven session update triggers a
	// (debounced) push without rewiring watchLoop. Sync-originated
	// writes are suppressed inside the watcher, so the loop is
	// already broken upstream.
	go func() {
		ch := h.subscribeGlobal()
		var debounce *time.Timer
		var dmu sync.Mutex
		trigger := func() {
			if !settings.syncCfg().Enabled || !syncClientHolder.Load().signedIn() {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := syncEngineHolder.Load().runSync(ctx); err != nil {
				log.Printf("sync push: %v", err)
			}
		}
		for range ch {
			dmu.Lock()
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(1500*time.Millisecond, trigger)
			dmu.Unlock()
		}
	}()
	// Sessions whose .md lives outside sessDir (after a move, or created
	// directly into a project repo) need their parent dir watched too.
	for _, s := range store.list() {
		dir := filepath.Dir(s.Path)
		if dir == sessDir {
			continue
		}
		if err := dw.ensure(dir); err != nil {
			log.Printf("watcher add %s: %v", dir, err)
		}
	}
	go watchLoop(w, store, h, prev, sessDir)

	mux := http.NewServeMux()
	registerTerminalRoutes(mux, store, termManager)
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	mux.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		data, _ := fs.ReadFile(sub, "favicon.svg")
		w.Write(data)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		w.Write(faviconBytes(sub))
	})
	// /api/files/<rel> serves files (typically images embedded via Obsidian's
	// `![[file.png]]`) from the configured sessions folder. Hidden segments
	// and traversal outside sessDir are rejected.
	sessDirClean := filepath.Clean(sessDir)
	mux.HandleFunc("/api/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method", 405)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, "/api/files/")
		if rel == "" {
			http.NotFound(w, r)
			return
		}
		decoded, err := url.PathUnescape(rel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		decoded = filepath.FromSlash(decoded)
		for _, seg := range strings.Split(decoded, string(filepath.Separator)) {
			if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}
		full := filepath.Clean(filepath.Join(sessDirClean, decoded))
		if full != sessDirClean && !strings.HasPrefix(full, sessDirClean+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, full)
	})

	mux.HandleFunc("/download/status-orchestrator.skill", func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(sub, "status-orchestrator.skill")
		if err != nil {
			http.Error(w, "skill bundle not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="status-orchestrator.skill"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Write(data)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		data, _ := fs.ReadFile(sub, "index.html")
		w.Write(data)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		writeJSON(w, map[string]any{
			"skillPath":       skillPath,
			"os":              runtimeOS(),
			"pathPlaceholder": pathPlaceholder(),
		})
	})

	mux.HandleFunc("/api/sync/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		writeJSON(w, syncEngineHolder.Load().status(settings.syncCfg()))
	})

	mux.HandleFunc("/api/sync/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		var body struct {
			Enabled         *bool   `json:"enabled,omitempty"`
			SupabaseURL     *string `json:"supabaseUrl,omitempty"`
			AnonKey         *string `json:"anonKey,omitempty"`
			AuthURLOverride *string `json:"authUrlOverride,omitempty"`
			Email           *string `json:"email,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		err := settings.updateSync(func(c *syncConfig) {
			if body.Enabled != nil {
				c.Enabled = *body.Enabled
			}
			if body.SupabaseURL != nil {
				c.SupabaseURL = strings.TrimSpace(*body.SupabaseURL)
			}
			if body.AnonKey != nil {
				c.AnonKey = strings.TrimSpace(*body.AnonKey)
			}
			if body.AuthURLOverride != nil {
				c.AuthURLOverride = strings.TrimSpace(*body.AuthURLOverride)
			}
			if body.Email != nil {
				c.Email = strings.TrimSpace(*body.Email)
			}
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Rebuild the client + engine with the new config and atomically
		// publish them so concurrent /api/sync/* handlers and the push
		// goroutine see a consistent pair. Tokens are reloaded from disk
		// by the constructor, so a sign-in survives a settings save.
		newClient := newSyncClient(settings.syncCfg(), filepath.Join(dataDir, "sync-auth.json"))
		newEngine := newSyncEngine(newClient, store, syncStateStore, syncSuppressor, sessDir, h, ensureWatch)
		syncClientHolder.Store(newClient)
		syncEngineHolder.Store(newEngine)
		writeJSON(w, newEngine.status(settings.syncCfg()))
	})

	mux.HandleFunc("/api/sync/otp/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		email := strings.TrimSpace(body.Email)
		if email == "" {
			http.Error(w, "email required", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := syncClientHolder.Load().otpStart(ctx, email); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		_ = settings.updateSync(func(c *syncConfig) { c.Email = email })
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/sync/otp/verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		var body struct {
			Email string `json:"email"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		email := strings.TrimSpace(body.Email)
		code := strings.TrimSpace(body.Code)
		if email == "" || code == "" {
			http.Error(w, "email and code required", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		client := syncClientHolder.Load()
		if err := client.otpVerify(ctx, email, code); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		// Validate RLS up front so a misconfigured project is caught
		// before any data round-trips. Failure leaves the sign-in
		// intact (so the user can re-run schema.sql and retry) but
		// surfaces a loud error.
		if err := client.rlsProbe(ctx); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		_ = settings.updateSync(func(c *syncConfig) { c.Enabled = true })
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := syncEngineHolder.Load().runSync(ctx); err != nil {
				log.Printf("first sync: %v", err)
			}
		}()
		writeJSON(w, syncEngineHolder.Load().status(settings.syncCfg()))
	})

	mux.HandleFunc("/api/sync/now", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		eng := syncEngineHolder.Load()
		if err := eng.runSync(ctx); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		writeJSON(w, eng.status(settings.syncCfg()))
	})

	mux.HandleFunc("/api/sync/signout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		if err := syncClientHolder.Load().clearTokens(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = settings.updateSync(func(c *syncConfig) { c.Enabled = false })
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", 405)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		writeJSON(w, fetchUpdateInfo(ctx))
	})

	mux.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		// Run the update in a goroutine so the request returns
		// immediately; the UI follows progress via /api/update/events.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := runSelfUpdate(ctx); err != nil {
				log.Printf("self-update: %v", err)
			}
		}()
		invalidateVersionCache()
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"started": true})
	})

	mux.HandleFunc("/api/update/events", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		ch := runner.subscribe()
		defer runner.unsubscribe(ch)
		ctx := r.Context()
		fmt.Fprintf(w, ": connected\n\n")
		f.Flush()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
				f.Flush()
			case p, ok := <-ch:
				if !ok {
					return
				}
				data, _ := json.Marshal(p)
				fmt.Fprintf(w, "data: %s\n\n", data)
				f.Flush()
			}
		}
	})

	mux.HandleFunc("/api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		folder, err := pickFolderNative()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if folder == "" {
			w.WriteHeader(204) // user cancelled
			return
		}
		writeJSON(w, map[string]any{"folder": folder})
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := h.subscribeGlobal()
		defer h.unsubscribeGlobal(ch)
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		ctx := r.Context()
		fmt.Fprintf(w, ": connected\n\n")
		f.Flush()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				fmt.Fprintf(w, "event: update\ndata: %s\n\n", msg)
				f.Flush()
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
				f.Flush()
			}
		}
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			list := store.list()
			out := make([]map[string]any, 0, len(list))
			for _, s := range list {
				out = append(out, sessionWithUpdated(s))
			}
			writeJSON(w, out)
		case http.MethodPost:
			var body struct {
				Title   string `json:"title"`
				Folder  string `json:"folder"`
				FileDir string `json:"file_dir"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			title := strings.TrimSpace(body.Title)
			if title == "" {
				title = "Session " + time.Now().Format("2006-01-02 15:04:05")
			}
			id := fmt.Sprintf("%d-%s", time.Now().Unix(), slug(title))
			// file_dir lets the caller (UI or skill) drop the .md straight
			// into a project repo so it can be committed without a later
			// move. Empty → historical default of sessDir.
			fileDir := strings.TrimSpace(body.FileDir)
			if fileDir == "" {
				fileDir = sessDir
			} else {
				if !filepath.IsAbs(fileDir) {
					http.Error(w, "file_dir must be absolute", 400)
					return
				}
				fileDir = filepath.Clean(fileDir)
				if err := os.MkdirAll(fileDir, 0755); err != nil {
					http.Error(w, "create file_dir: "+err.Error(), 500)
					return
				}
			}
			path := filepath.Join(fileDir, id+".md")
			// Skeleton per status-orchestrator SKILL §3: YAML front matter
			// (title, created, focus) plus the canonical section scaffold so
			// the dashboard renders with the expected shape on first open.
			// writeSessionRef() below preserves these fields when merging in
			// project_folder for the folder-provided case.
			initial := fmt.Sprintf(`---
title: %s
created: '%s'
focus: '(awaiting first request)'
---

## Active tasks

| # | Task | Agent | Started | Status |
|---|------|-------|---------|--------|

## Done (awaiting confirmation)

| # | Task | Finished | Result | Tested |
|---|------|----------|--------|--------|

## Completed

| # | Task | Confirmed | Result |
|---|------|-----------|--------|

## Blocked / needs input

_(empty unless a task is stuck)_

## Agent log

_(append-only, newest first)_

## Notes

_(free-form scratchpad — decisions, links, constraints)_
`, yamlQuote(title), time.Now().Format(time.RFC3339))
			// Register the session BEFORE touching the filesystem so the
			// fsnotify Create event sees it via byPath() and the auto-
			// discovery branch in watchLoop doesn't add a duplicate.
			sess := Session{ID: id, Title: title, Path: path, Created: time.Now()}
			if err := store.add(sess); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
				_, _ = store.remove(id)
				http.Error(w, err.Error(), 500)
				return
			}
			folder := strings.TrimSpace(body.Folder)
			if folder != "" {
				if err := writeSessionRef(path, folder, ""); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
			// If the file landed outside sessDir, start watching its parent
			// so saves trigger SSE updates the same way they do in sessDir.
			if fileDir != sessDir {
				if err := dw.ensure(fileDir); err != nil {
					log.Printf("watcher add %s: %v", fileDir, err)
				}
			}
			writeJSON(w, sessionWithUpdated(sess))
		default:
			http.Error(w, "method", 405)
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) > 1 {
			action = parts[1]
		}
		switch action {
		case "":
			switch r.Method {
			case http.MethodDelete:
				sess, err := store.remove(id)
				if err != nil {
					http.Error(w, err.Error(), 404)
					return
				}
				dir := filepath.Dir(sess.Path)
				os.Remove(sess.Path)
				prev.forget(id)
				if dir != sessDir {
					dw.release(dir)
				}
				w.WriteHeader(204)
			case http.MethodPatch:
				var body struct {
					Title  *string `json:"title,omitempty"`
					Pinned *bool   `json:"pinned,omitempty"`
					Folder *string `json:"folder,omitempty"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				if body.Title == nil && body.Pinned == nil && body.Folder == nil {
					http.Error(w, "nothing to update", 400)
					return
				}
				if body.Title != nil {
					t := strings.TrimSpace(*body.Title)
					if t == "" {
						http.Error(w, "title cannot be empty", 400)
						return
					}
					sess, ok := store.byID(id)
					if !ok {
						http.Error(w, "not found", 404)
						return
					}
					if err := setFileTitle(sess.Path, t); err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					// Keep the store's Title in sync so legacy reads/backups
					// don't drift. It's no longer authoritative — titleFor()
					// reads the file — but there's no reason to let it rot.
					_ = store.update(id, func(s *Session) { s.Title = t })
				}
				if body.Pinned != nil {
					if err := store.update(id, func(s *Session) { s.Pinned = *body.Pinned }); err != nil {
						http.Error(w, err.Error(), 404)
						return
					}
				}
				if body.Folder != nil {
					sess, ok := store.byID(id)
					if !ok {
						http.Error(w, "not found", 404)
						return
					}
					_, existingClaude, _ := parseSessionRef(sess.Path)
					if err := writeSessionRef(sess.Path, strings.TrimSpace(*body.Folder), existingClaude); err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
				}
				w.WriteHeader(204)
			default:
				http.Error(w, "method", 405)
			}
		case "move":
			// Relocate the session's .md file to a new directory (typically
			// a tracked git repo, so the file can be committed). The basename
			// is preserved; the destination dir is created if missing. The
			// store's Path is updated atomically with the rename, and the
			// fsnotify watcher gains/loses dirs so live updates keep firing.
			//
			// Body: {"dir": "<absolute destination directory>"}
			//
			// Caveats: attachments embedded via `![[file.png]]` still resolve
			// against sessDir (the configured sessions folder), so put them
			// there if you need them rendered.
			if r.Method != http.MethodPost {
				http.Error(w, "method", 405)
				return
			}
			var body struct {
				Dir string `json:"dir"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			destDir := strings.TrimSpace(body.Dir)
			if destDir == "" {
				http.Error(w, "dir required", 400)
				return
			}
			if !filepath.IsAbs(destDir) {
				http.Error(w, "dir must be absolute", 400)
				return
			}
			destDir = filepath.Clean(destDir)
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			oldPath := sess.Path
			oldDir := filepath.Dir(oldPath)
			if oldDir == destDir {
				writeJSON(w, sessionWithUpdated(sess))
				return
			}
			if err := os.MkdirAll(destDir, 0755); err != nil {
				http.Error(w, "create dest: "+err.Error(), 500)
				return
			}
			newPath := filepath.Join(destDir, filepath.Base(oldPath))
			if _, err := os.Stat(newPath); err == nil {
				http.Error(w, "destination already exists: "+newPath, 409)
				return
			}
			// Mute the fsnotify Remove that os.Rename will emit for oldPath
			// — we're moving the file deliberately, not deleting the
			// session, so the watchLoop must not tombstone it.
			syncSuppressor.suppressRemove(oldPath)
			// os.Rename is atomic within a filesystem. Across filesystems it
			// fails with EXDEV; fall back to copy+remove so a move into a
			// repo on a different mount still works.
			if err := os.Rename(oldPath, newPath); err != nil {
				if data, rerr := os.ReadFile(oldPath); rerr == nil {
					if werr := os.WriteFile(newPath, data, 0644); werr == nil {
						_ = os.Remove(oldPath)
					} else {
						http.Error(w, "copy: "+werr.Error(), 500)
						return
					}
				} else {
					http.Error(w, "rename: "+err.Error(), 500)
					return
				}
			}
			if err := store.update(id, func(s *Session) {
				s.Path = newPath
				s.RelPath = relPathFor(newPath, sessDir)
			}); err != nil {
				// Roll the file back so store and disk stay in sync.
				_ = os.Rename(newPath, oldPath)
				http.Error(w, err.Error(), 500)
				return
			}
			// Watch the new dir before releasing the old one so a save
			// during the swap is never lost. sessDir is permanent; never
			// release it (its initial refcount keeps it pinned anyway).
			if destDir != sessDir {
				if err := dw.ensure(destDir); err != nil {
					log.Printf("watcher add %s: %v", destDir, err)
				}
			}
			if oldDir != sessDir {
				dw.release(oldDir)
			}
			updated, _ := store.byID(id)
			// Push a dashboard event so any open tab swaps the displayed path
			// without needing the user to re-select the session.
			h.publishGlobal(buildGlobalEvent(updated, newPath))
			writeJSON(w, sessionWithUpdated(updated))
		case "archive":
			if r.Method != http.MethodPost {
				http.Error(w, "method", 405)
				return
			}
			var body struct {
				Archived bool `json:"archived"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if err := store.update(id, func(s *Session) { s.Archived = body.Archived }); err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			w.WriteHeader(204)
		case "content":
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			htmlOut, src, err := renderMDWithSource(sess.Path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Seed the prev cache with current content so a page refresh
			// doesn't cause the next update to flag everything as "new".
			prev.set(id, src)
			folder, claudeSession, focus, _ := parseSessionMeta(sess.Path)
			writeJSON(w, map[string]any{
				"html":          htmlOut,
				"path":          sess.Path,
				"title":         sess.Title,
				"updated":       fileModTime(sess.Path),
				"folder":        folder,
				"claudeSession": claudeSession,
				"focus":         focus,
			})
		case "meta":
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			folder, claudeSession, focus, _ := parseSessionMeta(sess.Path)
			writeJSON(w, map[string]any{
				"folder":        folder,
				"claudeSession": claudeSession,
				"focus":         focus,
				"metadata":      parseAllFrontMatter(sess.Path),
			})
		case "open":
			if r.Method != http.MethodPost {
				http.Error(w, "method", 405)
				return
			}
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			// Delegate to the platform's default-handler invocation
			// (`cmd /c start` on Windows, `xdg-open` on Linux, `open` on macOS).
			if err := openFileInDefaultApp(sess.Path); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		case "open-cmd":
			if r.Method != http.MethodPost {
				http.Error(w, "method", 405)
				return
			}
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			folder, claudeSession, _, _ := parseSessionMeta(sess.Path)
			folder = strings.TrimSpace(folder)
			if folder == "" {
				http.Error(w, "session has no folder", 400)
				return
			}
			// Resume existing Claude session when we have a UUID; otherwise
			// hand over the orchestrator prompt so Claude loads the skill.
			var claudeArgs []string
			if claudeSession != "" {
				claudeArgs = []string{"--resume", claudeSession}
			} else {
				claudeArgs = []string{freshClaudePrompt(sess.Path)}
			}
			if err := openShellInFolder(folder, claudeArgs); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		case "stream":
			sess, ok := store.byID(id)
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			f, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", 500)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")

			if payload := buildUpdate(sess.Path, sess.ID, prev); payload != "" {
				fmt.Fprintf(w, "event: update\ndata: %s\n\n", payload)
				f.Flush()
			}

			ch := h.subscribe(id)
			defer h.unsubscribe(id, ch)
			ping := time.NewTicker(25 * time.Second)
			defer ping.Stop()
			ctx := r.Context()
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-ch:
					fmt.Fprintf(w, "event: update\ndata: %s\n\n", msg)
					f.Flush()
				case <-ping.C:
					fmt.Fprintf(w, ": ping\n\n")
					f.Flush()
				}
			}
		default:
			http.Error(w, "not found", 404)
		}
	})

	log.Printf("status-updates listening on %s  (root=%s)", appURL, rootAbs)
	return http.Serve(ln, mux)
}

// dirWatcher refcounts fsnotify watches per directory so the same path can
// be ensured by multiple sessions (or by startup + a later move) without
// double-Adding, and so dirs are released when the last session leaves.
// Not safe for concurrent use — callers (HTTP handlers, watchLoop) hold
// the store mutex implicitly via short-lived ops; if we ever go truly
// concurrent here, wrap in a Mutex.
type dirWatcher struct {
	mu     sync.Mutex
	w      *fsnotify.Watcher
	counts map[string]int
}

func newDirWatcher(w *fsnotify.Watcher) *dirWatcher {
	return &dirWatcher{w: w, counts: map[string]int{}}
}

func (d *dirWatcher) ensure(dir string) error {
	dir = filepath.Clean(dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.counts[dir] > 0 {
		d.counts[dir]++
		return nil
	}
	if err := d.w.Add(dir); err != nil {
		return err
	}
	d.counts[dir] = 1
	return nil
}

func (d *dirWatcher) release(dir string) {
	dir = filepath.Clean(dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.counts[dir]
	if c <= 0 {
		return
	}
	if c > 1 {
		d.counts[dir] = c - 1
		return
	}
	delete(d.counts, dir)
	_ = d.w.Remove(dir)
}

func watchLoop(w *fsnotify.Watcher, store *Store, h *hub, prev *prevCache, sessDir string) {
	debounce := map[string]*time.Timer{}
	var dmu sync.Mutex
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if !isTrackedSessionFile(ev.Name) {
				continue
			}
			p := ev.Name
			// Remove/Rename: turn into a tombstone so the deletion
			// propagates to other devices. Skip if syncState says we
			// just wrote-and-removed-then-rewrote ourselves (e.g. an
			// atomic rewrite via .tmp + rename).
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if syncSuppressor.consumeRemove(p) {
					continue
				}
				if _, err := os.Stat(p); err == nil {
					// Path still exists (rename-over-self); ignore.
					continue
				}
				if sess, ok := store.byPath(p); ok {
					if _, err := store.remove(sess.ID); err == nil {
						h.publishGlobal(buildGlobalEvent(sess, p))
					}
				}
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			dmu.Lock()
			if t := debounce[p]; t != nil {
				t.Stop()
			}
			debounce[p] = time.AfterFunc(80*time.Millisecond, func() {
				// Read the body once so we can both compute a hash and
				// short-circuit the no-change case (e.g. when an editor
				// touches a file's mtime without changing bytes, or when
				// the sync engine just wrote this exact content).
				body, err := os.ReadFile(p)
				if err != nil {
					return
				}
				newHash := hashBytes(body)
				if syncSuppressor != nil && syncSuppressor.consume(p, newHash) {
					return
				}
				sess, ok := store.byPath(p)
				if !ok {
					// Tombstoned at this RelPath? Resurrect rather than
					// minting a fresh ID so other devices see continuity.
					if rel := relPathFor(p, sessDir); rel != "" {
						if t, ok := store.tombstoneByRelPath(rel); ok {
							if err := store.resurrect(t.ID, p, string(body)); err == nil {
								sess, _ = store.byID(t.ID)
								goto Publish
							}
						}
					}
					// New file dropped into the sessions folder by an
					// external editor (Obsidian etc.) — pull it into the
					// store so the rest of the pipeline can publish it.
					ns, err := registerDiscoveredFile(p, store)
					if err != nil {
						log.Printf("auto-discover %s: %v", p, err)
						return
					}
					sess = ns
				}
				if sess.BodyHash != newHash {
					_ = store.update(sess.ID, func(s *Session) {
						s.BodyHash = newHash
						if s.RelPath == "" {
							s.RelPath = relPathFor(p, sessDir)
						}
					})
					sess, _ = store.byID(sess.ID)
				}
			Publish:
				if payload := buildUpdate(p, sess.ID, prev); payload != "" {
					h.publish(sess.ID, payload)
				}
				h.publishGlobal(buildGlobalEvent(sess, p))
			})
			dmu.Unlock()
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Println("watcher:", err)
		}
	}
}

// isTrackedSessionFile returns true when the path's basename is a non-hidden
// `.md` file. Hidden files/folders (anything starting with `.`) and non-md
// files are ignored so vault config dirs like `.obsidian/` and arbitrary
// attachments don't trigger session updates.
func isTrackedSessionFile(p string) bool {
	base := filepath.Base(p)
	if base == "" || strings.HasPrefix(base, ".") {
		return false
	}
	return strings.EqualFold(filepath.Ext(base), ".md")
}

// discoverSessions walks sessDir recursively and registers any non-hidden
// `.md` file not yet in the store. Returns the count added. Subdirs are
// included so cloud-pushed sessions whose `rel_path` contains slashes
// (e.g. `archive/2026/plan.md`) are visible after a sync. Hidden dirs
// (e.g. `.obsidian/`) are skipped via fs.SkipDir.
func discoverSessions(sessDir string, store *Store) int {
	added := 0
	_ = filepath.WalkDir(sessDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := filepath.Base(path)
		if d.IsDir() {
			if path != sessDir && strings.HasPrefix(base, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !isTrackedSessionFile(path) {
			return nil
		}
		if _, ok := store.byPath(path); ok {
			return nil
		}
		if _, err := registerDiscoveredFile(path, store); err != nil {
			log.Printf("auto-discover %s: %v", path, err)
			return nil
		}
		added++
		return nil
	})
	return added
}

// watchSubdirs walks sessDir recursively and refcounts every non-hidden
// directory into the watcher. Without this, fsnotify only fires for the
// top-level sessions folder; nested sessions (created locally OR pulled
// from sync as `archive/2026/plan.md`) would never publish updates and
// the sync engine's local edits would never push back.
func watchSubdirs(sessDir string, dw *dirWatcher) {
	_ = filepath.WalkDir(sessDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if path != sessDir && strings.HasPrefix(base, ".") {
			return fs.SkipDir
		}
		if path == sessDir {
			return nil // already pinned by caller
		}
		if err := dw.ensure(path); err != nil {
			log.Printf("watcher add %s: %v", path, err)
		}
		return nil
	})
}

// registerDiscoveredFile creates a Session for an existing markdown file and
// adds it to the store. Created-time tracks the file's mtime so the sidebar
// orders newly imported notes alongside dashboard-created ones sensibly.
func registerDiscoveredFile(path string, store *Store) (Session, error) {
	if _, ok := store.byPath(path); ok {
		s, _ := store.byPath(path)
		return s, nil
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	title := fileTitle(path)
	if title == "" {
		title = stem
	}
	created := fileModTime(path)
	if created.IsZero() {
		created = time.Now()
	}
	id := fmt.Sprintf("%d-%s", created.Unix(), slug(stem))
	sess := Session{ID: id, Title: title, Path: path, Created: created}
	if err := store.add(sess); err != nil {
		return Session{}, err
	}
	return sess, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// runtimeOS returns a short OS identifier for the UI to branch on
// (e.g. path-placeholder shape, button copy).
func runtimeOS() string {
	return runtime.GOOS
}

// listenWithUpdateGrace is net.Listen with a small retry window when
// AI_STATUS_PORT_WAIT is set. The Windows self-update flow spawns the
// new exe before our process has fully exited; the child waits up to
// AI_STATUS_PORT_WAIT seconds for the port to free up rather than
// bailing out as a duplicate-instance.
func listenWithUpdateGrace(addr string) (net.Listener, error) {
	wait := os.Getenv("AI_STATUS_PORT_WAIT")
	if wait == "" {
		return net.Listen("tcp", addr)
	}
	secs, _ := strconv.Atoi(wait)
	if secs <= 0 {
		secs = 10
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("listen %s timed out", addr)
	}
	return nil, lastErr
}
