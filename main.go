package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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
}

type Store struct {
	mu       sync.Mutex
	Sessions []Session `json:"sessions"`
	file     string
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, s)
}

func (s *Store) save() error {
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
	s.Sessions = append(s.Sessions, sess)
	return s.save()
}

func (s *Store) update(id string, fn func(*Session)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Sessions {
		if s.Sessions[i].ID == id {
			fn(&s.Sessions[i])
			return s.save()
		}
	}
	return fmt.Errorf("not found: %s", id)
}

func (s *Store) remove(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sess := range s.Sessions {
		if sess.ID == id {
			s.Sessions = append(s.Sessions[:i], s.Sessions[i+1:]...)
			return sess, s.save()
		}
	}
	return Session{}, fmt.Errorf("not found: %s", id)
}

func (s *Store) list() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, len(s.Sessions))
	copy(out, s.Sessions)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.After(out[j].Created)
	})
	return out
}

func (s *Store) byPath(p string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.Sessions {
		if strings.EqualFold(sess.Path, p) {
			return sess, true
		}
	}
	return Session{}, false
}

func (s *Store) byID(id string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.Sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return Session{}, false
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
	goldmark.WithExtensions(extension.GFM, extension.Typographer, meta.Meta),
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
func buildGlobalEvent(sess Session, path string) string {
	htmlOut, _ := renderMD(path)
	b, _ := json.Marshal(map[string]any{
		"sessionId": sess.ID,
		"title":     titleFor(sess),
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
	sessDir := filepath.Join(rootAbs, "sessions")
	dataDir := filepath.Join(rootAbs, "data")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}

	store := &Store{file: filepath.Join(dataDir, "sessions.json")}
	if err := store.load(); err != nil {
		return err
	}

	// Write the embedded SKILL.md to disk so `claude` can read it by path
	// without requiring the user to have the skill pre-installed. Overwrite
	// on every start so binary upgrades always refresh the on-disk copy.
	skillPath = filepath.Join(dataDir, "status-orchestrator.SKILL.md")
	if err := os.WriteFile(skillPath, skillFile, 0644); err != nil {
		log.Printf("skill write: %v", err)
	}

	h := newHub()
	prev := newPrevCache()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(sessDir); err != nil {
		return err
	}
	go watchLoop(w, store, h, prev)

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
				Title  string `json:"title"`
				Folder string `json:"folder"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			title := strings.TrimSpace(body.Title)
			if title == "" {
				title = "Session " + time.Now().Format("2006-01-02 15:04:05")
			}
			id := fmt.Sprintf("%d-%s", time.Now().Unix(), slug(title))
			path := filepath.Join(sessDir, id+".md")
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
			if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
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
			sess := Session{ID: id, Title: title, Path: path, Created: time.Now()}
			if err := store.add(sess); err != nil {
				http.Error(w, err.Error(), 500)
				return
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
				os.Remove(sess.Path)
				prev.forget(id)
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

func watchLoop(w *fsnotify.Watcher, store *Store, h *hub, prev *prevCache) {
	debounce := map[string]*time.Timer{}
	var dmu sync.Mutex
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			p := ev.Name
			dmu.Lock()
			if t := debounce[p]; t != nil {
				t.Stop()
			}
			debounce[p] = time.AfterFunc(80*time.Millisecond, func() {
				sess, ok := store.byPath(p)
				if !ok {
					return
				}
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
