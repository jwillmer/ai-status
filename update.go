package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Phase labels emitted on /api/update/events. Constants instead of free
// strings so a typo at an emit site doesn't silently misrender in the UI.
const (
	phaseStarting     = "starting"
	phasePreconditions = "preconditions"
	phasePulling      = "pulling"
	phaseBuilding     = "building"
	phaseRestarting   = "restarting"
	phasePull         = "pull"
	phaseBuild        = "build"
	phaseRelaunch     = "relaunch"
)

type updateInfo struct {
	Version         string `json:"version"`
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Behind          int    `json:"behind"`
	LatestMessage   string `json:"latestMessage"`
	CanSelfUpdate   bool   `json:"canSelfUpdate"`
	Reason          string `json:"reason"`
	CompareURL      string `json:"compareURL"`
	RepoURL         string `json:"repoURL"`
	CheckedAt       int64  `json:"checkedAt"`
}

type updateProgress struct {
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
	Done   bool   `json:"done"`
}

// versionCacheTTL keeps refresh churn well under GitHub's 60 req/h
// anonymous quota even with several tabs open.
const versionCacheTTL = 10 * time.Minute

type versionCache struct {
	mu        sync.Mutex
	info      updateInfo
	fetchedAt time.Time
}

var verCache versionCache

// updateRunner serialises self-update attempts. Only one update runs at
// a time; concurrent triggers get a "busy" error. Subscribers get the
// most recent progress event on connect (so a tab opened mid-update sees
// the current banner state) and live events thereafter.
type updateRunner struct {
	mu      sync.Mutex
	running bool
	subs    map[chan updateProgress]struct{}
	last    *updateProgress
}

var runner = &updateRunner{
	subs: map[chan updateProgress]struct{}{},
}

func (r *updateRunner) subscribe() chan updateProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan updateProgress, 4)
	if r.last != nil {
		ch <- *r.last
	}
	r.subs[ch] = struct{}{}
	return ch
}

func (r *updateRunner) unsubscribe(ch chan updateProgress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[ch]; ok {
		delete(r.subs, ch)
		close(ch)
	}
}

func (r *updateRunner) emit(p updateProgress) {
	r.mu.Lock()
	r.last = &p
	subs := make([]chan updateProgress, 0, len(r.subs))
	for ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (r *updateRunner) fail(phase string, err error) error {
	r.emit(updateProgress{Phase: phase, Error: err.Error(), Done: true})
	return err
}

// resolvedExe returns the running binary's symlink-resolved path and its
// containing directory (which is expected to be the git repo root since
// the workflow is `go build` inside the checkout).
func resolvedExe() (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, filepath.Dir(exe), nil
}

type gitInfo struct {
	IsRepo  bool
	Clean   bool
	Branch  string
	HeadSHA string
	Reason  string
}

// inspectGit reads the local checkout state with two subprocess calls:
// `git rev-parse HEAD` doubles as a "is this a usable git checkout"
// probe (clear non-zero exit if .git is missing or git is off PATH), and
// `git status --porcelain --branch` returns dirty/clean plus the branch
// name in one go.
func inspectGit(root string) gitInfo {
	gi := gitInfo{}
	out, err := runCmd(root, "git", "rev-parse", "HEAD")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			gi.Reason = "git not on PATH"
		} else {
			gi.Reason = "binary is not running from a git checkout"
		}
		return gi
	}
	gi.IsRepo = true
	gi.HeadSHA = strings.TrimSpace(out)

	if out, err := runCmd(root, "git", "status", "--porcelain", "--branch"); err == nil {
		clean := true
		for i, line := range strings.Split(out, "\n") {
			if i == 0 {
				// "## main...origin/main" — extract the local branch name.
				rest := strings.TrimPrefix(line, "## ")
				if idx := strings.IndexAny(rest, ".\n"); idx >= 0 {
					gi.Branch = rest[:idx]
				} else {
					gi.Branch = rest
				}
				continue
			}
			if strings.TrimSpace(line) != "" {
				clean = false
			}
		}
		gi.Clean = clean
	}
	return gi
}

// runCmd runs cmd in dir and returns stdout. Errors include the program
// name, args, and a snippet of output so callers can log them as-is —
// callers should NOT re-concatenate the returned stdout into the error
// (it's already there).
func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func fetchUpdateInfo(ctx context.Context) updateInfo {
	verCache.mu.Lock()
	if !verCache.fetchedAt.IsZero() && time.Since(verCache.fetchedAt) < versionCacheTTL {
		info := verCache.info
		verCache.mu.Unlock()
		return info
	}
	verCache.mu.Unlock()

	info := updateInfo{
		Version:   Version,
		Current:   CommitSHA,
		RepoURL:   "https://github.com/" + repoSlug,
		CheckedAt: time.Now().Unix(),
	}

	_, root, err := resolvedExe()
	if err != nil {
		info.Reason = "could not locate binary directory: " + err.Error()
		return cacheAndReturn(info)
	}

	gi := inspectGit(root)
	if info.Current == "" && gi.HeadSHA != "" {
		info.Current = gi.HeadSHA
	}

	if err := fetchUpstream(ctx, &info); err != nil {
		info.Reason = "github lookup failed: " + err.Error()
		return cacheAndReturn(info)
	}

	info.UpdateAvailable = info.Current != "" && info.Latest != "" &&
		!strings.HasPrefix(info.Latest, info.Current) &&
		!strings.HasPrefix(info.Current, info.Latest)

	if info.UpdateAvailable {
		info.CompareURL = fmt.Sprintf("https://github.com/%s/compare/%s...%s", repoSlug, info.Current, info.Latest)
	}

	info.CanSelfUpdate, info.Reason = selfUpdatePreconditions(gi, info)
	if info.Reason == "" && !info.UpdateAvailable {
		info.Reason = "already up to date"
	}

	return cacheAndReturn(info)
}

func cacheAndReturn(info updateInfo) updateInfo {
	verCache.mu.Lock()
	verCache.info = info
	verCache.fetchedAt = time.Now()
	verCache.mu.Unlock()
	return info
}

// invalidateVersionCache forces the next /api/version call to re-fetch.
// Called right after a successful update kickoff so the page reload
// (triggered by the conn-banner) sees the post-restart state.
func invalidateVersionCache() {
	verCache.mu.Lock()
	verCache.fetchedAt = time.Time{}
	verCache.mu.Unlock()
}

func selfUpdatePreconditions(gi gitInfo, info updateInfo) (bool, string) {
	if !info.UpdateAvailable {
		return false, ""
	}
	if !gi.IsRepo {
		return false, gi.Reason
	}
	if gi.HeadSHA == "" {
		return false, "could not read local HEAD"
	}
	if !gi.Clean {
		return false, "working tree has uncommitted changes — pull manually to keep your work"
	}
	if gi.Branch != "" && gi.Branch != "main" && gi.Branch != "HEAD" {
		return false, fmt.Sprintf("local branch is %q (not main) — switch to main or pull manually", gi.Branch)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return false, "go toolchain required for self-update"
	}
	return true, ""
}

// fetchUpstream populates info.Latest, info.LatestMessage, and info.Behind
// from a single GitHub API call when we know the local SHA — /compare
// returns the head commit (main's tip) plus ahead_by in one response.
// When the local SHA is unknown we can't form a compare URL, so fall back
// to /commits/main and leave Behind at 0.
func fetchUpstream(ctx context.Context, info *updateInfo) error {
	if info.Current == "" {
		sha, msg, err := fetchLatestCommit(ctx)
		if err != nil {
			return err
		}
		info.Latest = sha
		info.LatestMessage = msg
		return nil
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/compare/%s...main", repoSlug, info.Current)
	var payload struct {
		AheadBy int `json:"ahead_by"`
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
		BaseCommit struct {
			SHA string `json:"sha"`
		} `json:"base_commit"`
	}
	if err := githubGet(ctx, url, &payload); err != nil {
		return err
	}
	info.Behind = payload.AheadBy
	if n := len(payload.Commits); n > 0 {
		head := payload.Commits[n-1]
		info.Latest = head.SHA
		info.LatestMessage = strings.SplitN(head.Commit.Message, "\n", 2)[0]
	} else {
		// AheadBy == 0 → main is at our SHA (no commits between).
		info.Latest = info.Current
	}
	return nil
}

func fetchLatestCommit(ctx context.Context) (string, string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits/main", repoSlug)
	var payload struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := githubGet(ctx, url, &payload); err != nil {
		return "", "", err
	}
	msg := strings.SplitN(payload.Commit.Message, "\n", 2)[0]
	return payload.SHA, msg, nil
}

func githubGet(ctx context.Context, url string, dst any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ai-status/"+Version)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

var httpClient = &http.Client{Timeout: 8 * time.Second}

// runSelfUpdate claims the runner lock, pulls, builds, and re-execs into
// the new binary. On the success path the function never returns: the
// process is replaced (Unix) or exits after spawning the child (Windows).
func runSelfUpdate(ctx context.Context) error {
	r := runner
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("update already in progress")
	}
	r.running = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	r.emit(updateProgress{Phase: phaseStarting})

	exe, root, err := resolvedExe()
	if err != nil {
		return r.fail(phaseStarting, err)
	}

	info := fetchUpdateInfo(ctx)
	if !info.CanSelfUpdate {
		reason := info.Reason
		if reason == "" {
			reason = "preconditions not met"
		}
		return r.fail(phasePreconditions, errors.New(reason))
	}

	r.emit(updateProgress{Phase: phasePulling, Detail: "git pull --ff-only origin main"})
	if _, err := runCmd(root, "git", "pull", "--ff-only", "origin", "main"); err != nil {
		return r.fail(phasePull, err)
	}

	newSHA := ""
	if out, err := runCmd(root, "git", "rev-parse", "HEAD"); err == nil {
		newSHA = strings.TrimSpace(out)
	}

	r.emit(updateProgress{Phase: phaseBuilding, Detail: "go build (this can take a few seconds)"})
	newPath := exe + ".new"
	_ = os.Remove(newPath)

	ldflags := fmt.Sprintf("-X main.Version=%s -X main.CommitSHA=%s", Version, newSHA)
	if runtime.GOOS == "windows" {
		ldflags = "-H windowsgui " + ldflags
	}
	if _, err := runCmd(root, "go", "build", "-ldflags="+ldflags, "-o", newPath, "."); err != nil {
		return r.fail(phaseBuild, err)
	}

	r.emit(updateProgress{Phase: phaseRestarting, Detail: "swapping binary and re-launching"})
	if err := swapAndRelaunch(exe, newPath); err != nil {
		return r.fail(phaseRelaunch, err)
	}
	return nil
}
