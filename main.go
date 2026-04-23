package main

import (
	"bytes"
	"embed"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/atotto/clipboard"
	"github.com/fsnotify/fsnotify"
	"github.com/getlantern/systray"
	"github.com/pkg/browser"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed static
var staticFS embed.FS

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
	goldmark.WithExtensions(extension.GFM, extension.Typographer),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

func renderMD(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := md.Convert(src, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func fileModTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// buildUpdate returns a JSON string with rendered HTML + file mtime.
func buildUpdate(path string) string {
	htmlOut, err := renderMD(path)
	if err != nil {
		return ""
	}
	b, _ := json.Marshal(map[string]any{
		"html":    htmlOut,
		"updated": fileModTime(path),
	})
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
	return map[string]any{
		"id":       s.ID,
		"title":    titleFor(s),
		"path":     s.Path,
		"created":  s.Created,
		"archived": s.Archived,
		"pinned":   s.Pinned,
		"updated":  fileModTime(s.Path),
	}
}

// fileTitle returns the first `# heading` line in the given markdown file,
// or "" if none is found within the first few kilobytes.
func fileTitle(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > 8192 {
		data = data[:8192]
	}
	for _, raw := range strings.Split(string(data), "\n") {
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

// setFileTitle rewrites the first `# heading` line of path to `# <newTitle>`.
// If no heading exists, prepends one.
func setFileTitle(path, newTitle string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
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

	ln, err := net.Listen("tcp", *addr)
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

	if !*noOpen {
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = browser.OpenURL(appURL)
		}()
	}

	if *noTray {
		if err := <-srvErr; err != nil {
			log.Fatal(err)
		}
		return
	}

	onReady := func() {
		systray.SetIcon(iconBytes())
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
					systray.Quit()
					return
				case err := <-srvErr:
					if err != nil {
						log.Println("server:", err)
					}
					systray.Quit()
					return
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

	h := newHub()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(sessDir); err != nil {
		return err
	}
	go watchLoop(w, store, h)

	mux := http.NewServeMux()
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
		w.Write(iconBytes())
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
				Title string `json:"title"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			title := strings.TrimSpace(body.Title)
			if title == "" {
				title = "Session " + time.Now().Format("2006-01-02 15:04:05")
			}
			id := fmt.Sprintf("%d-%s", time.Now().Unix(), slug(title))
			path := filepath.Join(sessDir, id+".md")
			initial := fmt.Sprintf("# %s\n\n_Created %s_\n\n", title, time.Now().Format(time.RFC3339))
			if err := os.WriteFile(path, []byte(initial), 0644); err != nil {
				http.Error(w, err.Error(), 500)
				return
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
				w.WriteHeader(204)
			case http.MethodPatch:
				var body struct {
					Title  *string `json:"title,omitempty"`
					Pinned *bool   `json:"pinned,omitempty"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				if body.Title == nil && body.Pinned == nil {
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
			htmlOut, err := renderMD(sess.Path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{
				"html":    htmlOut,
				"path":    sess.Path,
				"title":   sess.Title,
				"updated": fileModTime(sess.Path),
			})
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

			if payload := buildUpdate(sess.Path); payload != "" {
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

func watchLoop(w *fsnotify.Watcher, store *Store, h *hub) {
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
				if payload := buildUpdate(p); payload != "" {
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

// iconBytes returns an ICO wrapping the embedded 32x32 tray-icon.png.
// Used for the system-tray icon on Windows and as /favicon.ico fallback.
func iconBytes() []byte {
	sub, _ := fs.Sub(staticFS, "static")
	pngData, err := fs.ReadFile(sub, "tray-icon.png")
	if err != nil || len(pngData) == 0 {
		return nil
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0))            // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // type=icon
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // count
	ico.WriteByte(32)                                             // width
	ico.WriteByte(32)                                             // height
	ico.WriteByte(0)                                              // no palette
	ico.WriteByte(0)                                              // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // planes
	binary.Write(&ico, binary.LittleEndian, uint16(32))           // bpp
	binary.Write(&ico, binary.LittleEndian, uint32(len(pngData))) // size
	binary.Write(&ico, binary.LittleEndian, uint32(22))           // offset
	ico.Write(pngData)
	return ico.Bytes()
}
