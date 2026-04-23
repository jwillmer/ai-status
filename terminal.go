package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/UserExistsError/conpty"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v2"
)

// ---------- session-ref block (parse/write) ----------
//
// Current format: YAML front matter at the very top of the file, fenced by
// `---` lines. goldmark-meta consumes it so it does not render to HTML.
//
//     ---
//     project_folder: C:\Projects\GitHub\ai-status
//     claude_session: <uuid>
//     created: 2026-04-23T10:07:18+02:00
//     ---
//
//     # <title>
//     ...body...
//
// Legacy format (still parsed for backward compat): an HTML comment block
// named `<!-- status-orchestrator:session-ref ... -->`. New writes always use
// YAML front matter.

const legacySessionRefStart = "<!-- status-orchestrator:session-ref"
const legacySessionRefEnd = "-->"

// Legacy HTML-comment parsers, retained for backward compat on un-migrated files.
var legacyProjectFolderLineRe = regexp.MustCompile(`(?m)^\s*project_folder:\s*(.*?)\s*$`)
var legacyFolderLineRe = regexp.MustCompile(`(?m)^\s*folder:\s*(.*?)\s*$`)
var legacyClaudeLineRe = regexp.MustCompile(`(?m)^\s*claude_session:\s*(.*?)\s*$`)

// yamlFrontMatter models the fields we care about in the leading `---` block.
// Accepts both `project_folder:` (canonical) and `folder:` (legacy alias);
// project_folder wins when both are present.
type yamlFrontMatter struct {
	ProjectFolder string `yaml:"project_folder"`
	Folder        string `yaml:"folder"`
	ClaudeSession string `yaml:"claude_session"`
	Created       string `yaml:"created"`
	Focus         string `yaml:"focus"`
}

// splitFrontMatter returns (frontMatterBody, rest, ok). If the file starts
// with a YAML front-matter block (`---` fence as first non-blank line, closed
// by another `---` line), frontMatterBody holds the YAML between the fences
// and rest holds everything after the closing fence. Otherwise ok=false.
const utf8BOM = "\xef\xbb\xbf"

func splitFrontMatter(src string) (fm, rest string, ok bool) {
	// Skip an optional UTF-8 BOM at the very start.
	s := strings.TrimPrefix(src, utf8BOM)
	// Find the first non-blank line.
	i := 0
	for i < len(s) {
		// Find next newline
		nl := strings.IndexByte(s[i:], '\n')
		var line string
		if nl < 0 {
			line = s[i:]
		} else {
			line = s[i : i+nl]
		}
		if strings.TrimSpace(line) != "" {
			if strings.TrimRight(line, "\r") != "---" {
				return "", "", false
			}
			// Move past the fence line (and its newline).
			if nl < 0 {
				return "", "", false
			}
			start := i + nl + 1
			// Find the closing `---` line.
			j := start
			for j < len(s) {
				nl2 := strings.IndexByte(s[j:], '\n')
				var l2 string
				var next int
				if nl2 < 0 {
					l2 = s[j:]
					next = len(s)
				} else {
					l2 = s[j : j+nl2]
					next = j + nl2 + 1
				}
				if strings.TrimRight(l2, "\r") == "---" {
					return s[start:j], s[next:], true
				}
				j = next
			}
			return "", "", false
		}
		if nl < 0 {
			return "", "", false
		}
		i += nl + 1
	}
	return "", "", false
}

// parseSessionRef reads the first ~4KB of path and extracts folder +
// claude_session. YAML front matter is the primary source; the legacy HTML
// comment block is the fallback for un-migrated files. Missing fields
// return "".
func parseSessionRef(path string) (folder, claudeSession string, err error) {
	folder, claudeSession, _, err = parseSessionMeta(path)
	return
}

// parseAllFrontMatter returns the full YAML front matter of a session file
// as an ordered list of key/value pairs (order preserved from source). The
// UI "Show metadata" toggle uses this so *all* frontmatter fields surface,
// not just the handful parseSessionMeta pulls out. Returns nil if the file
// has no front matter or fails to parse.
func parseAllFrontMatter(path string) []map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(bufio.NewReader(f), buf)
	head := string(buf[:n])
	fm, _, ok := splitFrontMatter(head)
	if !ok {
		return nil
	}
	// Scan lines in source order — gives stable ordering matching the file.
	// Accepts simple `key: value` pairs (no nested YAML). Values retain
	// whatever quoting the source used, with surrounding quotes stripped.
	var out []map[string]string
	for _, raw := range strings.Split(fm, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, "'\"")
		out = append(out, map[string]string{"key": key, "value": val})
	}
	return out
}

// parseSessionMeta extends parseSessionRef with `focus` from the YAML
// frontmatter. Kept separate so legacy call sites don't need to change.
func parseSessionMeta(path string) (folder, claudeSession, focus string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", "", err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(bufio.NewReader(f), buf)
	head := string(buf[:n])

	// Primary: YAML front matter.
	if fm, _, ok := splitFrontMatter(head); ok {
		var meta yamlFrontMatter
		if yerr := yaml.Unmarshal([]byte(fm), &meta); yerr == nil {
			folder = strings.TrimSpace(meta.ProjectFolder)
			if folder == "" {
				folder = strings.TrimSpace(meta.Folder)
			}
			claudeSession = strings.TrimSpace(meta.ClaudeSession)
			focus = strings.TrimSpace(meta.Focus)
			return folder, claudeSession, focus, nil
		}
		// Fall through to legacy if YAML parse fails.
	}

	// Fallback: legacy HTML-comment block.
	start := strings.Index(head, legacySessionRefStart)
	if start < 0 {
		return "", "", "", nil
	}
	rest := head[start+len(legacySessionRefStart):]
	end := strings.Index(rest, legacySessionRefEnd)
	if end < 0 {
		return "", "", "", nil
	}
	block := rest[:end]
	if m := legacyProjectFolderLineRe.FindStringSubmatch(block); len(m) == 2 {
		folder = strings.TrimSpace(m[1])
	}
	if folder == "" {
		if m := legacyFolderLineRe.FindStringSubmatch(block); len(m) == 2 {
			folder = strings.TrimSpace(m[1])
		}
	}
	if m := legacyClaudeLineRe.FindStringSubmatch(block); len(m) == 2 {
		claudeSession = strings.TrimSpace(m[1])
	}
	return folder, claudeSession, "", nil
}

// writeSessionRef writes or updates the YAML front matter block at the top
// of path. If a YAML block is already present, its fields are merged in
// (existing `created` is preserved; legacy HTML-comment blocks are dropped).
// If no YAML block is present, one is prepended.
//
// `claude_session` is omitted when empty. `project_folder` is always written.
// Values are quoted with single quotes to survive Windows backslash paths and
// other YAML edge cases.
func writeSessionRef(path, folder, claudeSession string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)

	// Try to pick up existing created timestamp (from YAML or legacy block)
	// so we don't lose it on rewrite.
	existingCreated := ""
	if fm, _, ok := splitFrontMatter(text); ok {
		var meta yamlFrontMatter
		if yaml.Unmarshal([]byte(fm), &meta) == nil {
			existingCreated = strings.TrimSpace(meta.Created)
		}
	}

	body := text
	// If YAML front matter exists, strip it — we're about to re-emit one.
	if _, rest, ok := splitFrontMatter(text); ok {
		body = rest
		// Trim exactly one leading blank line (we'll re-insert separator below).
		body = strings.TrimPrefix(body, "\n")
		body = strings.TrimPrefix(body, "\r\n")
	}

	// If a legacy HTML-comment block exists anywhere in the head, strip it
	// (plus a trailing blank line) as part of the migration.
	if start := strings.Index(body, legacySessionRefStart); start >= 0 {
		rest := body[start+len(legacySessionRefStart):]
		if endRel := strings.Index(rest, legacySessionRefEnd); endRel >= 0 {
			end := start + len(legacySessionRefStart) + endRel + len(legacySessionRefEnd)
			// Also consume the blank line that usually follows.
			tail := body[end:]
			tail = strings.TrimPrefix(tail, "\n")
			tail = strings.TrimPrefix(tail, "\r\n")
			body = body[:start] + tail
		}
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("project_folder: ")
	b.WriteString(yamlQuote(folder))
	b.WriteString("\n")
	if claudeSession != "" {
		b.WriteString("claude_session: ")
		b.WriteString(yamlQuote(claudeSession))
		b.WriteString("\n")
	}
	if existingCreated != "" {
		b.WriteString("created: ")
		b.WriteString(yamlQuote(existingCreated))
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// yamlQuote wraps a scalar in single quotes, escaping embedded single quotes
// per YAML spec (double them). Safe for Windows paths containing `\`.
func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ---------- terminal manager ----------

const ringCap = 64 * 1024

type Terminal struct {
	mu          sync.Mutex
	cpty        *conpty.ConPty
	folder      string
	ring        []byte
	subs        map[chan []byte]struct{}
	done        bool
	exitCode    int
	cancel      context.CancelFunc
}

type TerminalManager struct {
	mu   sync.Mutex
	term map[string]*Terminal
}

func newTerminalManager() *TerminalManager {
	return &TerminalManager{term: map[string]*Terminal{}}
}

func (m *TerminalManager) get(sessionID string) *Terminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.term[sessionID]
}

func (m *TerminalManager) Start(sessionID, folder string, cols, rows int) error {
	if folder == "" {
		return fmt.Errorf("folder is empty")
	}
	m.mu.Lock()
	if t, ok := m.term[sessionID]; ok {
		t.mu.Lock()
		running := !t.done
		t.mu.Unlock()
		if running {
			m.mu.Unlock()
			return fmt.Errorf("terminal already running")
		}
		// prune stale entry
		delete(m.term, sessionID)
	}
	m.mu.Unlock()

	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	cpty, err := conpty.Start("cmd.exe",
		conpty.ConPtyWorkDir(folder),
		conpty.ConPtyDimensions(cols, rows))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Terminal{
		cpty:   cpty,
		folder: folder,
		ring:   make([]byte, 0, ringCap),
		subs:   map[chan []byte]struct{}{},
		cancel: cancel,
	}
	m.mu.Lock()
	m.term[sessionID] = t
	m.mu.Unlock()

	go t.readLoop(sessionID)
	go t.waitLoop(ctx, sessionID)
	return nil
}

func (t *Terminal) appendRing(p []byte) {
	if len(p) >= ringCap {
		t.ring = append(t.ring[:0], p[len(p)-ringCap:]...)
		return
	}
	total := len(t.ring) + len(p)
	if total <= ringCap {
		t.ring = append(t.ring, p...)
		return
	}
	drop := total - ringCap
	t.ring = append(t.ring[:0], t.ring[drop:]...)
	t.ring = append(t.ring, p...)
}

func (t *Terminal) readLoop(sessionID string) {
	buf := make([]byte, 4096)
	for {
		n, err := t.cpty.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			t.mu.Lock()
			t.appendRing(chunk)
			for ch := range t.subs {
				select {
				case ch <- chunk:
				default:
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (t *Terminal) waitLoop(ctx context.Context, sessionID string) {
	code, err := t.cpty.Wait(ctx)
	t.mu.Lock()
	t.done = true
	if err == nil {
		t.exitCode = int(code)
	} else {
		t.exitCode = -1
	}
	// close subscribers so WS handlers notice exit
	for ch := range t.subs {
		close(ch)
		delete(t.subs, ch)
	}
	t.mu.Unlock()
}

func (m *TerminalManager) Write(sessionID string, data []byte) error {
	t := m.get(sessionID)
	if t == nil {
		return fmt.Errorf("no terminal")
	}
	t.mu.Lock()
	done := t.done
	cpty := t.cpty
	t.mu.Unlock()
	if done {
		return fmt.Errorf("terminal exited")
	}
	_, err := cpty.Write(data)
	return err
}

func (m *TerminalManager) Resize(sessionID string, cols, rows int) error {
	t := m.get(sessionID)
	if t == nil {
		return fmt.Errorf("no terminal")
	}
	t.mu.Lock()
	done := t.done
	cpty := t.cpty
	t.mu.Unlock()
	if done {
		return fmt.Errorf("terminal exited")
	}
	return cpty.Resize(cols, rows)
}

func (m *TerminalManager) Kill(sessionID string) error {
	t := m.get(sessionID)
	if t == nil {
		return fmt.Errorf("no terminal")
	}
	t.mu.Lock()
	done := t.done
	cancel := t.cancel
	cpty := t.cpty
	t.mu.Unlock()
	if done {
		return nil
	}
	// Cancel the Wait context and close the ConPty exactly once. The readLoop
	// will observe EOF and exit; waitLoop will finalize exitCode and close
	// subscribers. waitLoop no longer calls cpty.Close() — that would be a
	// double-close and previously crashed the process.
	if cancel != nil {
		cancel()
	}
	return cpty.Close()
}

// Status returns running, exitCode, hasTerminal.
func (m *TerminalManager) Status(sessionID string) (bool, int, bool) {
	t := m.get(sessionID)
	if t == nil {
		return false, 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done, t.exitCode, true
}

// Subscribe returns current scrollback + a channel receiving future output chunks.
// The returned cancel removes the subscription. Channel is closed when terminal exits.
func (m *TerminalManager) Subscribe(sessionID string) ([]byte, <-chan []byte, func(), error) {
	t := m.get(sessionID)
	if t == nil {
		return nil, nil, nil, fmt.Errorf("no terminal")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	scroll := make([]byte, len(t.ring))
	copy(scroll, t.ring)
	if t.done {
		ch := make(chan []byte)
		close(ch)
		return scroll, ch, func() {}, nil
	}
	ch := make(chan []byte, 32)
	t.subs[ch] = struct{}{}
	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, ok := t.subs[ch]; ok {
			delete(t.subs, ch)
			close(ch)
		}
	}
	return scroll, ch, cancel, nil
}

func (m *TerminalManager) KillAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.term))
	for id := range m.term {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Kill(id)
	}
}

// ---------- HTTP routes ----------

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// registerTerminalRoutes wires terminal + meta routes onto mux.
// The /api/sessions/ handler in main.go delegates to handleSessionSubroute for
// terminal-specific actions by calling into manager.
func registerTerminalRoutes(mux *http.ServeMux, store *Store, manager *TerminalManager) {
	mux.HandleFunc("/api/terminal/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/terminal/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		switch sub {
		case "":
			switch r.Method {
			case http.MethodPost:
				handleTerminalStart(w, r, id, store, manager)
			case http.MethodDelete:
				if err := manager.Kill(id); err != nil {
					http.Error(w, err.Error(), 404)
					return
				}
				w.WriteHeader(204)
			default:
				http.Error(w, "method", 405)
			}
		case "status":
			if r.Method != http.MethodGet {
				http.Error(w, "method", 405)
				return
			}
			running, code, has := manager.Status(id)
			folder, claudeSession := "", ""
			if sess, ok := store.byID(id); ok {
				folder, claudeSession, _ = parseSessionRef(sess.Path)
			}
			writeJSON(w, map[string]any{
				"running":       running,
				"exitCode":      code,
				"hasTerminal":   has,
				"folder":        folder,
				"claudeSession": claudeSession,
			})
		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc("/ws/terminal/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/ws/terminal/")
		id = strings.TrimSuffix(id, "/")
		if id == "" {
			http.Error(w, "missing id", 400)
			return
		}
		handleTerminalWS(w, r, id, manager)
	})
}

func handleTerminalStart(w http.ResponseWriter, r *http.Request, id string, store *Store, manager *TerminalManager) {
	var body struct {
		Cols    int    `json:"cols"`
		Rows    int    `json:"rows"`
		Command string `json:"command"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	sess, ok := store.byID(id)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	folder, _, _ := parseSessionRef(sess.Path)
	folder = strings.TrimSpace(folder)
	if folder == "" {
		http.Error(w, "session has no folder", 400)
		return
	}
	if err := manager.Start(id, folder, body.Cols, body.Rows); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if cmd := strings.TrimSpace(body.Command); cmd != "" {
		go func() {
			time.Sleep(150 * time.Millisecond)
			if err := manager.Write(id, []byte(cmd+"\r")); err != nil {
				log.Printf("terminal initial command write: %v", err)
			}
		}()
	}
	writeJSON(w, map[string]any{"running": true})
}

func handleTerminalWS(w http.ResponseWriter, r *http.Request, id string, manager *TerminalManager) {
	scroll, ch, cancel, err := manager.Subscribe(id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		cancel()
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()
	defer cancel()

	if len(scroll) > 0 {
		if err := conn.WriteMessage(websocket.BinaryMessage, scroll); err != nil {
			return
		}
	}

	// Reader goroutine: client → pty input / resize
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if err := manager.Write(id, data); err != nil {
					return
				}
			case websocket.TextMessage:
				var msg struct {
					Type string `json:"type"`
					Cols int    `json:"cols"`
					Rows int    `json:"rows"`
				}
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}
				if msg.Type == "resize" {
					_ = manager.Resize(id, msg.Cols, msg.Rows)
				}
				// unknown types ignored
			}
		}
	}()

	// Writer loop: pty output → client
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				// terminal exited
				_, code, _ := manager.Status(id)
				payload, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
				_ = conn.WriteMessage(websocket.TextMessage, payload)
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "exit"),
					time.Now().Add(time.Second))
				return
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
				return
			}
		case <-readerDone:
			return
		}
	}
}
