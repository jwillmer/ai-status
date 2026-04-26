const $ = (s) => document.querySelector(s);
const listActive = $("#list-active");
const listArchived = $("#list-archived");
const archCount = $("#arch-count");
const searchInput = $("#search");
const viewHead = $("#view-head");
const viewTitle = $("#view-title");
const viewPath = $("#view-path");
const viewUpdated = $("#view-updated");
const viewEl = $("#view");
const archBtn = $("#arch-btn");
const delBtn = $("#del-btn");
const showCmdBtn = $("#show-cmd-btn");
const openCmdBtn = $("#open-cmd-btn");
const openFileBtn = $("#open-file-btn");
const metaToggleBtn = $("#meta-toggle-btn");
const metaPanel = $("#meta-panel");
const viewFocus = $("#view-focus");
const newBtn = $("#new-btn");
const helpBtn = $("#help-btn");
const helpDialog = $("#help-dialog");
const helpClose = $("#help-close");
const notifyBtn = $("#notify-btn");
const themeBtn = $("#theme-btn");
const menuToggle = $("#menu-toggle");
const menuOverlay = $("#menu-overlay");
const archHead = document.querySelector(".arch-head");
const connBanner = $("#conn-banner");

const mainEl = document.querySelector("main");
const splitEl = $("#split");
const splitDivider = $("#split-divider");
const termPanel = $("#term-panel");
const termHost = $("#term-host");
const termEmpty = $("#term-empty");
const termFolderForm = $("#term-folder-form");
const termFolderInput = $("#term-folder-input");

const newDialog = $("#new-dialog");
const newForm = $("#new-form");
const newTitleInput = $("#new-title");
const newFolderInput = $("#new-folder");
const newFolderBrowse = $("#new-folder-browse");
const newFolderPicker = $("#new-folder-picker");
const newCancelBtn = $("#new-cancel");

const folderDialog = $("#folder-dialog");
const folderForm = $("#folder-form");
const folderInput = $("#folder-input");
const folderBrowse = $("#folder-browse");
const folderPicker = $("#folder-picker");
const folderCancelBtn = $("#folder-cancel");

let sessions = [];
let currentId = null;
let currentUpdated = null; // Date of current session last update
let es = null;
let archOpen = false;
let searchQuery = "";

function sessionSortKey(s) {
  // Compare by epoch ms so that timestamps written with different TZ
  // offsets (server local vs UTC "Z" from toISOString) sort consistently.
  const t = s.updated || s.created;
  if (!t) return 0;
  const n = Date.parse(t);
  return Number.isFinite(n) ? n : 0;
}
function sortSessions(arr) {
  return arr.slice().sort((a, b) => {
    const pa = a.pinned ? 1 : 0;
    const pb = b.pinned ? 1 : 0;
    if (pa !== pb) return pb - pa;
    return sessionSortKey(b) - sessionSortKey(a);
  });
}

async function api(path, opts = {}) {
  const r = await fetch(path, {
    headers: { "content-type": "application/json" },
    ...opts,
  });
  if (!r.ok && r.status !== 204) throw new Error(await r.text());
  if (r.status === 204) return null;
  return r.json();
}

function fmtDate(iso) {
  const d = new Date(iso);
  const today = new Date();
  const same = d.toDateString() === today.toDateString();
  if (same) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

function fmtRelative(date) {
  if (!date) return "never";
  const ms = Date.now() - date.getTime();
  const s = Math.max(0, Math.round(ms / 1000));
  // Coarse buckets — avoids a distracting per-second clock. The UI tick
  // below is 30s, matching the minute-level resolution.
  if (s < 10) return "just now";
  if (s < 60) return "a moment ago";
  if (s < 120) return "a minute ago";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} minutes ago`;
  const h = Math.round(m / 60);
  if (h < 2) return "an hour ago";
  if (h < 24) return `${h} hours ago`;
  const d = Math.round(h / 24);
  if (d < 2) return "yesterday";
  if (d < 7) return `${d} days ago`;
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}

function fmtAbsolute(date) {
  if (!date) return "";
  return date.toLocaleString();
}

function renderList() {
  const q = searchQuery.trim().toLowerCase();
  const match = (s) => !q || (s.title || "").toLowerCase().includes(q);

  const activeAll = sortSessions(sessions.filter(s => !s.archived));
  const archivedAll = sortSessions(sessions.filter(s => s.archived));
  const active = activeAll.filter(match);
  const archived = archivedAll.filter(match);

  if (q) {
    archCount.textContent = archivedAll.length
      ? `(${archived.length}/${archivedAll.length})`
      : "";
  } else {
    archCount.textContent = archivedAll.length ? `(${archivedAll.length})` : "";
  }
  listArchived.hidden = !archOpen;

  const build = (ul, arr, emptyLabel) => {
    ul.innerHTML = "";
    if (!arr.length) {
      const li = document.createElement("li");
      li.className = "item-meta";
      li.style.cursor = "default";
      li.textContent = emptyLabel;
      ul.appendChild(li);
      return;
    }
    for (const s of arr) {
      const li = document.createElement("li");
      li.dataset.id = s.id;
      if (s.id === currentId) li.classList.add("active");
      if (s.pinned) li.classList.add("pinned");
      const updated = s.updated ? new Date(s.updated) : null;

      const titleRow = document.createElement("div");
      titleRow.className = "item-title-row";
      const pinBtn = document.createElement("button");
      pinBtn.className = "pin-btn" + (s.pinned ? " pinned" : "");
      pinBtn.type = "button";
      pinBtn.textContent = s.pinned ? "★" : "☆";
      pinBtn.title = s.pinned ? "Unpin" : "Pin";
      pinBtn.setAttribute("aria-label", s.pinned ? "Unpin session" : "Pin session");
      pinBtn.onclick = (e) => {
        e.stopPropagation();
        togglePin(s.id);
      };
      const titleEl = document.createElement("span");
      titleEl.className = "item-title";
      titleEl.textContent = s.title;
      titleRow.appendChild(titleEl);
      titleRow.appendChild(pinBtn);

      const metaRow = document.createElement("span");
      metaRow.className = "item-meta";
      const metaUpd = document.createElement("span");
      metaUpd.className = "item-updated";
      metaUpd.textContent = "upd. " + fmtRelative(updated);
      metaUpd.title = updated ? fmtAbsolute(updated) : "";
      metaRow.appendChild(metaUpd);

      li.appendChild(titleRow);
      li.appendChild(metaRow);
      li.onclick = () => openSession(s.id);
      ul.appendChild(li);
    }
  };

  const emptyActive = q
    ? (activeAll.length ? "No matches" : "No active sessions")
    : "No active sessions";
  const emptyArch = q ? "No matches" : "None";
  build(listActive, active, emptyActive);
  build(listArchived, archived, emptyArch);
}

async function togglePin(id) {
  const s = sessions.find(x => x.id === id);
  if (!s) return;
  const next = !s.pinned;
  try {
    await api(`/api/sessions/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ pinned: next }),
    });
    s.pinned = next;
    renderList();
  } catch (e) {
    alert("Pin failed: " + e.message);
  }
}

function updateUpdatedPill() {
  if (!currentUpdated) {
    viewUpdated.textContent = "";
    viewUpdated.title = "";
    viewUpdated.classList.remove("fresh", "stale");
    return;
  }
  viewUpdated.textContent = "Updated " + fmtRelative(currentUpdated);
  viewUpdated.title = fmtAbsolute(currentUpdated);
  const ageSec = (Date.now() - currentUpdated.getTime()) / 1000;
  viewUpdated.classList.toggle("fresh", ageSec < 10);
  viewUpdated.classList.toggle("stale", ageSec > 300);
}

// tick relative times every 30s — matches the minute-level resolution of
// fmtRelative(), no need for per-second updates.
setInterval(() => {
  updateUpdatedPill();
  for (const li of document.querySelectorAll("#sidebar li[data-id]")) {
    const s = sessions.find(x => x.id === li.dataset.id);
    if (!s) continue;
    const meta = li.querySelector(".item-updated");
    if (!meta) continue;
    const updated = s.updated ? new Date(s.updated) : null;
    meta.textContent = "upd. " + fmtRelative(updated);
  }
}, 30000);

async function loadSessions() {
  sessions = await api("/api/sessions");
  renderList();
  if (!currentId) {
    const top = sortSessions(sessions.filter(s => !s.archived))[0];
    if (top) openSession(top.id);
  }
}

function openNewDialog() {
  if (!newDialog) return;
  newTitleInput.value = "Session " + new Date().toLocaleString();
  newFolderInput.value = "";
  // Select title text so user can immediately overtype
  queueMicrotask(() => {
    newTitleInput.focus();
    newTitleInput.select();
  });
  if (typeof newDialog.showModal === "function") newDialog.showModal();
  else newDialog.setAttribute("open", "");
}

async function submitNewSession(includeFolder) {
  const title = (newTitleInput.value || "").trim();
  if (!title) { newTitleInput.focus(); return; }
  const body = { title };
  if (includeFolder) {
    const folder = (newFolderInput.value || "").trim();
    if (folder) body.folder = folder;
  }
  try {
    const sess = await api("/api/sessions", {
      method: "POST",
      body: JSON.stringify(body),
    });
    sessions.unshift(sess);
    newDialog.close();
    renderList();
    openSession(sess.id);
  } catch (e) {
    alert("Create failed: " + e.message);
  }
}

function createSession() {
  openNewDialog();
}

if (newForm) {
  newForm.addEventListener("submit", (e) => {
    e.preventDefault();
    submitNewSession(true);
  });
}
if (newCancelBtn) newCancelBtn.onclick = () => newDialog.close();
async function pickFolderNative() {
  // Backend spawns the Windows FolderBrowserDialog so we get a real absolute
  // path instead of the sandboxed folder-name-only that <input webkitdirectory>
  // gives. 204 → user cancelled.
  const res = await fetch("/api/pick-folder", { method: "POST" });
  if (res.status === 204) return "";
  if (!res.ok) throw new Error(`pick-folder failed: ${res.status}`);
  const data = await res.json();
  return data.folder || "";
}

if (newFolderBrowse) {
  newFolderBrowse.onclick = async () => {
    try {
      const folder = await pickFolderNative();
      if (folder) newFolderInput.value = folder;
    } catch (e) {
      alert("Browse failed: " + e.message);
    }
  };
}
if (newDialog) {
  // Click outside to close
  newDialog.addEventListener("click", (e) => {
    if (e.target === newDialog) {
      const r = newDialog.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
        newDialog.close();
      }
    }
  });
}

function closeStream() {
  if (es) { es.close(); es = null; }
}

function applyUpdate(data) {
  try {
    const payload = typeof data === "string" ? JSON.parse(data) : data;
    viewEl.innerHTML = payload.html || '<div class="empty">(empty file)</div>';
    if (typeof payload.focus === "string") updateFocus(payload.focus);
    currentUpdated = payload.updated ? new Date(payload.updated) : new Date();
    updateUpdatedPill();
    // reflect in sidebar model
    const s = sessions.find(x => x.id === currentId);
    if (s) {
      s.updated = currentUpdated.toISOString();
      renderList();
    }
    // Notifications are fired from the global stream (/api/events) so they
    // cover every session, not just the currently-viewed one.
  } catch (e) {
    console.error("bad update payload", e, data);
  }
}

const DEFAULT_DOC_TITLE = "AI Status";
let unseen = false;

// Both favicons inlined as data URIs so the browser tab icon never depends
// on the server being reachable — when the app is offline, the last-rendered
// icon stays put instead of the browser falling back to its generic tile.
const FAVICON_ICON_PATH = '<path fill-rule="evenodd" clip-rule="evenodd" d="M11.9426 1.25H12.0574C14.3658 1.24999 16.1748 1.24998 17.5863 1.43975C19.031 1.63399 20.1711 2.03933 21.0659 2.93414C21.9607 3.82895 22.366 4.96897 22.5603 6.41371C22.75 7.82519 22.75 9.63423 22.75 11.9426V12.0574C22.75 14.3658 22.75 16.1748 22.5603 17.5863C22.366 19.031 21.9607 20.1711 21.0659 21.0659C20.1711 21.9607 19.031 22.366 17.5863 22.5603C16.1748 22.75 14.3658 22.75 12.0574 22.75H11.9426C9.63423 22.75 7.82519 22.75 6.41371 22.5603C4.96897 22.366 3.82895 21.9607 2.93414 21.0659C2.03933 20.1711 1.63399 19.031 1.43975 17.5863C1.24998 16.1748 1.24999 14.3658 1.25 12.0574V11.9426C1.24999 9.63423 1.24998 7.82519 1.43975 6.41371C1.63399 4.96897 2.03933 3.82895 2.93414 2.93414C3.82895 2.03933 4.96897 1.63399 6.41371 1.43975C7.82519 1.24998 9.63423 1.24999 11.9426 1.25ZM6.61358 2.92637C5.33517 3.09825 4.56445 3.42514 3.9948 3.9948C3.42514 4.56445 3.09825 5.33517 2.92637 6.61358C2.75159 7.91356 2.75 9.62177 2.75 12C2.75 14.3782 2.75159 16.0864 2.92637 17.3864C3.09825 18.6648 3.42514 19.4355 3.9948 20.0052C4.56445 20.5749 5.33517 20.9018 6.61358 21.0736C7.91356 21.2484 9.62177 21.25 12 21.25C14.3782 21.25 16.0864 21.2484 17.3864 21.0736C18.6648 20.9018 19.4355 20.5749 20.0052 20.0052C20.5749 19.4355 20.9018 18.6648 21.0736 17.3864C21.2484 16.0864 21.25 14.3782 21.25 12C21.25 9.62177 21.2484 7.91356 21.0736 6.61358C20.9018 5.33517 20.5749 4.56445 20.0052 3.9948C19.4355 3.42514 18.6648 3.09825 17.3864 2.92637C16.0864 2.75159 14.3782 2.75 12 2.75C9.62177 2.75 7.91356 2.75159 6.61358 2.92637ZM10.5172 6.4569C10.8172 6.74256 10.8288 7.21729 10.5431 7.51724L7.68596 10.5172C7.5444 10.6659 7.34812 10.75 7.14286 10.75C6.9376 10.75 6.74131 10.6659 6.59975 10.5172L5.4569 9.31724C5.17123 9.01729 5.18281 8.54256 5.48276 8.2569C5.78271 7.97123 6.25744 7.98281 6.5431 8.28276L7.14286 8.9125L9.4569 6.48276C9.74256 6.18281 10.2173 6.17123 10.5172 6.4569ZM12.25 9C12.25 8.58579 12.5858 8.25 13 8.25H18C18.4142 8.25 18.75 8.58579 18.75 9C18.75 9.41421 18.4142 9.75 18 9.75H13C12.5858 9.75 12.25 9.41421 12.25 9ZM10.5172 13.4569C10.8172 13.7426 10.8288 14.2173 10.5431 14.5172L7.68596 17.5172C7.5444 17.6659 7.34812 17.75 7.14286 17.75C6.9376 17.75 6.74131 17.6659 6.59975 17.5172L5.4569 16.3172C5.17123 16.0173 5.18281 15.5426 5.48276 15.2569C5.78271 14.9712 6.25744 14.9828 6.5431 15.2828L7.14286 15.9125L9.4569 13.4828C9.74256 13.1828 10.2173 13.1712 10.5172 13.4569ZM12.25 16C12.25 15.5858 12.5858 15.25 13 15.25H18C18.4142 15.25 18.75 15.5858 18.75 16C18.75 16.4142 18.4142 16.75 18 16.75H13C12.5858 16.75 12.25 16.4142 12.25 16Z" fill="#7aa2ff"/>';
const FAVICON_NORMAL = "data:image/svg+xml;utf8," + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none">' +
  FAVICON_ICON_PATH +
  '</svg>'
);
const FAVICON_BADGE = "data:image/svg+xml;utf8," + encodeURIComponent(
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none">' +
  FAVICON_ICON_PATH +
  '<circle cx="20" cy="4" r="4" fill="#ff5a5a" stroke="#0f1115" stroke-width="1"/>' +
  '</svg>'
);

function setFavicon(href) {
  const link = document.querySelector('link[rel="icon"]');
  if (link) link.setAttribute("href", href);
}

function setDocTitle(sessionTitle) {
  document.title = sessionTitle || DEFAULT_DOC_TITLE;
}

function markUnseen() {
  if (unseen) return;
  unseen = true;
  setFavicon(FAVICON_BADGE);
}

function clearUnseen() {
  if (!unseen) return;
  unseen = false;
  setFavicon(FAVICON_NORMAL);
}

function isForeground() {
  return document.visibilityState === "visible" && document.hasFocus();
}

function onForegroundChange() {
  if (isForeground()) clearUnseen();
}

document.addEventListener("visibilitychange", onForegroundChange);
window.addEventListener("focus", onForegroundChange);
window.addEventListener("blur", () => { /* hook for future if needed */ });

function openSession(id) {
  currentId = id;
  const s = sessions.find(x => x.id === id);
  if (!s) return;
  // Auto-close the mobile drawer when the user picks a session — the sidebar
  // was dim/overlaid and they wouldn't want to tap again to dismiss it.
  if (isMobileView && isMobileView()) closeMenu();
  closeStream();
  clearUnseen();
  viewHead.hidden = false;
  viewTitle.textContent = s.title;
  setDocTitle(s.title);
  viewPath.textContent = s.path;
  archBtn.textContent = s.archived ? "Unarchive" : "Archive";
  currentUpdated = s.updated ? new Date(s.updated) : null;
  updateUpdatedPill();
  updateFocus(s.focus);
  updateOpenCmdVisibility();
  updateMetaPanelVisibility();
  renderList();
  viewEl.innerHTML = '<div class="empty">Loading…</div>';

  es = new EventSource(`/api/sessions/${id}/stream`);
  es.addEventListener("update", (e) => applyUpdate(e.data));
  es.onerror = () => { /* browser auto-reconnects */ };

  showTerminalFor(id);
  updateCmdCollapse();
}

async function toggleArchive() {
  if (!currentId) return;
  const s = sessions.find(x => x.id === currentId);
  if (!s) return;
  await api(`/api/sessions/${currentId}/archive`, {
    method: "POST",
    body: JSON.stringify({ archived: !s.archived }),
  });
  s.archived = !s.archived;
  archBtn.textContent = s.archived ? "Unarchive" : "Archive";
  renderList();
}

async function deleteSession() {
  if (!currentId) return;
  const s = sessions.find(x => x.id === currentId);
  if (!s) return;
  if (!confirm(`Delete "${s.title}"? The .md file will be removed.`)) return;
  await api(`/api/sessions/${currentId}`, { method: "DELETE" });
  const deletedId = currentId;
  sessions = sessions.filter(x => x.id !== deletedId);
  closeStream();
  currentId = null;
  viewHead.hidden = true;
  viewEl.innerHTML = '<div class="empty">Select or create a session.</div>';
  setDocTitle(null);
  // Tear down terminal for this session if any
  const entry = termEntry(deletedId);
  if (entry) {
    try { if (entry.ws) entry.ws.close(); } catch {}
    try { entry.term.dispose(); } catch {}
    try { entry.div.remove(); } catch {}
    terminals.delete(deletedId);
  }
  metaCache.delete(deletedId);
  if (currentTermId === deletedId) {
    currentTermId = null;
    hideAllTerminals();
    if (termEmpty) termEmpty.hidden = false;
    termFolderForm.hidden = true;
  }
  renderList();
  updateCmdCollapse();
}

async function copyPath() {
  if (!currentId) return;
  const s = sessions.find(x => x.id === currentId);
  if (!s) return;
  try {
    await navigator.clipboard.writeText(s.path);
  } catch {
    const r = document.createRange();
    r.selectNode(viewPath);
    getSelection().removeAllRanges();
    getSelection().addRange(r);
    document.execCommand?.("copy");
  }
  viewPath.classList.add("copied");
  clearTimeout(copyPath._t);
  copyPath._t = setTimeout(() => viewPath.classList.remove("copied"), 1100);
}

function startRename() {
  if (!currentId) return;
  if (viewTitle.isContentEditable) return;
  viewTitle.contentEditable = "true";
  viewTitle.focus();
  const r = document.createRange();
  r.selectNodeContents(viewTitle);
  const sel = getSelection();
  sel.removeAllRanges();
  sel.addRange(r);
}

async function commitRename() {
  if (!viewTitle.isContentEditable) return;
  viewTitle.contentEditable = "false";
  const s = sessions.find(x => x.id === currentId);
  if (!s) return;
  const title = viewTitle.textContent.trim();
  if (!title || title === s.title) {
    viewTitle.textContent = s.title;
    return;
  }
  try {
    await api(`/api/sessions/${currentId}`, {
      method: "PATCH",
      body: JSON.stringify({ title }),
    });
    s.title = title;
    if (s.id === currentId) setDocTitle(title);
    renderList();
    // Refresh metadata so the YAML `title:` shown in the metadata panel
    // reflects the rename without waiting for a file-watch roundtrip.
    await refreshMeta(currentId);
    updateMetaPanelVisibility();
  } catch (e) {
    viewTitle.textContent = s.title;
    alert("Rename failed: " + e.message);
  }
}

viewTitle.addEventListener("dblclick", startRename);
viewTitle.addEventListener("blur", commitRename);
viewTitle.addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); viewTitle.blur(); }
  if (e.key === "Escape") {
    const s = sessions.find(x => x.id === currentId);
    if (s) viewTitle.textContent = s.title;
    viewTitle.contentEditable = "false";
  }
});
viewPath.addEventListener("click", copyPath);

newBtn.onclick = createSession;
archBtn.onclick = toggleArchive;
delBtn.onclick = deleteSession;
archHead.onclick = () => { archOpen = !archOpen; renderList(); };

searchInput.addEventListener("input", () => {
  searchQuery = searchInput.value;
  renderList();
});

/* ---------------------------------------------------------------------------
 * Desktop notifications
 * Opt-in bell in the sidebar. Fires a native notification via the
 * Notifications API when an SSE update arrives for the open session AND the
 * tab is hidden. State persisted in localStorage.notifications_enabled.
 * No libraries, no service workers — just window Notifications.
 * ------------------------------------------------------------------------- */
const NOTIF_KEY = "notifications_enabled";
const NOTIF_THROTTLE_MS = 3000;
const notifLastFired = new Map(); // sessionId -> timestamp
let notifState = "off"; // "off" | "on" | "denied"
const notifSupported = typeof window !== "undefined" && "Notification" in window;

const ICONS = {
  bell: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/></svg>`,
  bellOff: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13.73 21a2 2 0 0 1-3.46 0"/><path d="M18.63 13A17.89 17.89 0 0 1 18 8"/><path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14"/><path d="M18 8a6 6 0 0 0-9.33-5"/><line x1="1" y1="1" x2="23" y2="23"/></svg>`,
  system: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>`,
  light: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>`,
  dark: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>`
};

function renderNotifyBtn() {
  if (!notifyBtn) return;
  notifyBtn.classList.remove("on", "denied");
  if (!notifSupported) {
    notifyBtn.classList.add("denied");
    notifyBtn.innerHTML = ICONS.bellOff;
    notifyBtn.title = "Notifications not supported in this browser";
    notifyBtn.setAttribute("aria-pressed", "false");
    return;
  }
  if (notifState === "denied") {
    notifyBtn.classList.add("denied");
    notifyBtn.innerHTML = ICONS.bellOff;
    notifyBtn.title = "Notifications blocked — check browser settings";
    notifyBtn.setAttribute("aria-pressed", "false");
  } else if (notifState === "on") {
    notifyBtn.classList.add("on");
    notifyBtn.innerHTML = ICONS.bell;
    notifyBtn.title = "Desktop notifications on (click to disable)";
    notifyBtn.setAttribute("aria-pressed", "true");
  } else {
    notifyBtn.innerHTML = ICONS.bellOff;
    notifyBtn.title = "Enable desktop notifications";
    notifyBtn.setAttribute("aria-pressed", "false");
  }
}

function initNotifyState() {
  if (!notifSupported) { notifState = "denied"; renderNotifyBtn(); return; }
  const stored = localStorage.getItem(NOTIF_KEY) === "true";
  const perm = Notification.permission;
  if (perm === "denied") {
    notifState = "denied";
    localStorage.removeItem(NOTIF_KEY);
  } else if (stored && perm === "granted") {
    notifState = "on";
  } else {
    notifState = "off";
    if (stored && perm !== "granted") localStorage.removeItem(NOTIF_KEY);
  }
  renderNotifyBtn();
}

async function onNotifyClick() {
  if (!notifSupported || notifState === "denied") return;
  if (notifState === "on") {
    notifState = "off";
    localStorage.setItem(NOTIF_KEY, "false");
    renderNotifyBtn();
    return;
  }
  // off -> request permission
  try {
    const perm = await Notification.requestPermission();
    if (perm === "granted") {
      notifState = "on";
      localStorage.setItem(NOTIF_KEY, "true");
    } else if (perm === "denied") {
      notifState = "denied";
      localStorage.removeItem(NOTIF_KEY);
      alert("Notifications are blocked. Enable them in your browser's site settings to receive alerts.");
    } else {
      notifState = "off";
    }
  } catch (e) {
    notifState = "off";
    console.error("Notification permission error", e);
  }
  renderNotifyBtn();
}

function htmlToPlainText(html) {
  const tmp = document.createElement("div");
  tmp.innerHTML = html;
  return (tmp.textContent || "").replace(/\s+/g, " ").trim();
}

// Fire a notification for any session update (from the global /api/events stream)
// when the tab is hidden. Throttled per-session.
function maybeNotifyAny(p) {
  if (notifState !== "on" || !notifSupported) return;
  if (Notification.permission !== "granted") {
    notifState = "off";
    localStorage.removeItem(NOTIF_KEY);
    renderNotifyBtn();
    return;
  }
  if (isForeground()) return;
  if (!p || !p.sessionId) return;
  const now = Date.now();
  const last = notifLastFired.get(p.sessionId) || 0;
  if (now - last < NOTIF_THROTTLE_MS) return;
  notifLastFired.set(p.sessionId, now);

  const title = (p.title || "Status update") + " updated";
  const body = p.snippet || "File changed.";
  try {
    const n = new Notification(title, {
      body,
      icon: "/favicon.svg",
      tag: p.sessionId,
    });
    n.onclick = () => {
      window.focus();
      n.close();
    };
  } catch (e) {
    console.error("Notification failed", e);
  }
}

// Global event stream: receives update metadata for every session, lets the
// sidebar auto-reorder and notifications fire for any session (not just the
// currently-viewed one).
let globalEs = null;
function startGlobalStream() {
  if (globalEs) globalEs.close();
  globalEs = new EventSource("/api/events");
  globalEs.addEventListener("update", (e) => {
    let p;
    try { p = JSON.parse(e.data); } catch { return; }
    const s = sessions.find(x => x.id === p.sessionId);
    if (s) {
      if (p.updated) s.updated = p.updated;
      if (p.title && p.title !== s.title) {
        s.title = p.title;
        if (p.sessionId === currentId) {
          viewTitle.textContent = p.title;
          setDocTitle(p.title);
        }
      }
      renderList();
    }
    if (p.sessionId === currentId && !isForeground()) {
      markUnseen();
    }
    maybeNotifyAny(p);
  });
  globalEs.onopen = () => markConnAlive();
  globalEs.onerror = () => markConnError();
}

/* ---------------------------------------------------------------------------
 * Connection banner
 * Sticky notice shown when the app server is unreachable so the user knows
 * live updates are paused. Driven by the always-on /api/events stream.
 * ------------------------------------------------------------------------- */
let connErrorTimer = null;
function markConnAlive() {
  if (connErrorTimer) { clearTimeout(connErrorTimer); connErrorTimer = null; }
  if (connBanner && !connBanner.hidden) {
    // Server was unreachable long enough to show the banner and is now back.
    // Most likely it was restarted — reload so the UI picks up any new static
    // assets (HTML/CSS/JS). Plain cached-asset case is cheap because the
    // server sets no-cache on /favicon.* and /api/*; /static/* is served by
    // http.FileServer which honours If-Modified-Since.
    location.reload();
    return;
  }
}
function markConnError() {
  if (!connBanner) return;
  if (connErrorTimer || !connBanner.hidden) return;
  // Grace period: browsers fire onerror during normal reconnect blips.
  connErrorTimer = setTimeout(() => {
    connErrorTimer = null;
    if (!globalEs || globalEs.readyState !== EventSource.OPEN) {
      connBanner.hidden = false;
    }
  }, 2500);
}


if (notifyBtn) notifyBtn.onclick = onNotifyClick;
initNotifyState();
startGlobalStream();
/* --- end notifications --- */

// Obsidian-style wikilinks render as <a class="wikilink" data-wikilink="…">.
// Resolve the target against the loaded sessions list (case-insensitive
// match against the filename stem) and switch to it. Unmatched targets
// no-op so a click on a stale link doesn't navigate the page somewhere
// unexpected.
viewEl.addEventListener("click", (e) => {
  const a = e.target.closest("a.wikilink");
  if (!a) return;
  e.preventDefault();
  const target = (a.dataset.wikilink || "").toLowerCase();
  if (!target) return;
  const match = sessions.find(s => {
    const base = (s.path || "").split(/[\\/]/).pop() || "";
    const stem = base.replace(/\.md$/i, "");
    return stem.toLowerCase() === target;
  });
  if (match) openSession(match.id);
});

helpBtn.onclick = () => helpDialog.showModal();
helpClose.onclick = () => helpDialog.close();
helpDialog.addEventListener("click", (e) => {
  const r = helpDialog.getBoundingClientRect();
  if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
    helpDialog.close();
  }
});

document.addEventListener("keydown", (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "n") {
    e.preventDefault();
    createSession();
  }
});

let appConfig = { skillPath: "", os: "windows", pathPlaceholder: "" };
async function loadConfig() {
  try { appConfig = await api(`/api/config`); } catch {}
  // Swap the folder-input placeholders so Linux/macOS users see a POSIX
  // path example instead of the Windows-shaped default in the HTML.
  if (appConfig && appConfig.pathPlaceholder) {
    document.querySelectorAll(
      "#new-folder, #term-folder-input, #folder-input"
    ).forEach((el) => { el.placeholder = appConfig.pathPlaceholder; });
  }
}

/* ---------------------------------------------------------------------------
 * Update check — on page load, ask the server whether origin/main has moved
 * past the running binary's commit. If it has, show a banner with either an
 * Update-now button (when preconditions allow self-update) or a GitHub link
 * (when they don't). Dismissals are remembered per-commit in localStorage so
 * one click silences the banner until the next upstream change.
 * ------------------------------------------------------------------------- */
const updateBanner = $("#update-banner");
const updateText = updateBanner ? updateBanner.querySelector(".update-text") : null;
const updateInstallBtn = $("#update-install");
const updateLink = $("#update-link");
const updateDismissBtn = $("#update-dismiss");
let dismissTarget = "";
let updateEvtSource = null;

function setBannerText(builder) {
  while (updateText.firstChild) updateText.removeChild(updateText.firstChild);
  builder({
    plain: (s) => updateText.appendChild(document.createTextNode(s)),
    strong: (s) => {
      const el = document.createElement("strong");
      el.textContent = s;
      updateText.appendChild(el);
    },
    code: (s) => {
      const el = document.createElement("code");
      el.textContent = s;
      updateText.appendChild(el);
    },
    muted: (s) => {
      const el = document.createElement("span");
      el.style.opacity = ".75";
      el.textContent = s;
      updateText.appendChild(el);
    },
  });
}

function renderUpdateBanner(info) {
  if (!updateBanner || !updateText) return;
  if (!info || !info.updateAvailable) { updateBanner.hidden = true; return; }
  let dismissed = false;
  try { dismissed = localStorage.getItem("update-dismissed") === info.latest; } catch {}
  if (dismissed) { updateBanner.hidden = true; return; }

  dismissTarget = info.latest;
  const behind = info.behind > 0
    ? `${info.behind} commit${info.behind === 1 ? "" : "s"} behind`
    : "update available";

  setBannerText(({ strong, plain, code, muted }) => {
    strong("New version");
    plain(` · ${behind} (latest `);
    code((info.latest || "").slice(0, 7));
    plain(")");
    if (info.latestMessage) plain(` — ${info.latestMessage}`);
    if (!info.canSelfUpdate && info.reason) {
      plain(" ");
      muted(`(${info.reason})`);
    }
  });

  updateInstallBtn.hidden = !info.canSelfUpdate;
  updateInstallBtn.disabled = false;
  updateInstallBtn.textContent = "Update now";
  if (updateLink) {
    updateLink.href = info.compareURL || info.repoURL || "#";
    updateLink.hidden = false;
  }
  updateBanner.classList.remove("is-error");
  updateBanner.hidden = false;
}

function showUpdateProgress(phase, detail) {
  if (!updateBanner || !updateText) return;
  updateBanner.hidden = false;
  updateBanner.classList.remove("is-error");
  if (updateInstallBtn) {
    updateInstallBtn.disabled = true;
    updateInstallBtn.textContent = phase + "…";
  }
  setBannerText(({ strong, plain, muted }) => {
    strong("Updating");
    plain(` · ${phase}`);
    if (detail) { plain(" "); muted(detail); }
  });
}

function showUpdateError(phase, error) {
  if (!updateBanner || !updateText) return;
  updateBanner.hidden = false;
  updateBanner.classList.add("is-error");
  if (updateInstallBtn) updateInstallBtn.hidden = true;
  setBannerText(({ strong, plain }) => {
    strong("Update failed");
    plain(` · ${phase}: ${error}`);
  });
}

async function checkForUpdate() {
  if (!updateBanner) return;
  // Silent on failure — conn-banner already covers "server unreachable".
  try { renderUpdateBanner(await api(`/api/version`)); } catch {}
}

function closeUpdateStream() {
  if (!updateEvtSource) return;
  updateEvtSource.close();
  updateEvtSource = null;
}

function startUpdateStream() {
  if (updateEvtSource) return;
  try {
    updateEvtSource = new EventSource("/api/update/events");
  } catch { return; }
  updateEvtSource.onmessage = (e) => {
    let p; try { p = JSON.parse(e.data); } catch { return; }
    if (p.error) { showUpdateError(p.phase || "update", p.error); closeUpdateStream(); return; }
    if (p.phase) showUpdateProgress(p.phase, p.detail || "");
    // On a clean restart the SSE drops; conn-banner's reload-on-reconnect
    // takes the page from here.
    if (p.done) closeUpdateStream();
  };
  updateEvtSource.onerror = closeUpdateStream;
}

async function startSelfUpdate() {
  if (!updateInstallBtn) return;
  updateInstallBtn.disabled = true;
  updateInstallBtn.textContent = "Starting…";
  startUpdateStream();
  try {
    await api("/api/update", { method: "POST" });
  } catch (e) {
    showUpdateError("request", e && e.message ? e.message : String(e));
  }
}

if (updateInstallBtn) updateInstallBtn.onclick = startSelfUpdate;
if (updateDismissBtn) updateDismissBtn.onclick = () => {
  if (dismissTarget) {
    try { localStorage.setItem("update-dismissed", dismissTarget); } catch {}
  }
  if (updateBanner) updateBanner.hidden = true;
};

/* ---------------------------------------------------------------------------
 * Theme switcher — cycles system → light → dark → system. Persisted as the
 * `theme` cookie (1 year). The initial paint is already applied by the inline
 * script in index.html <head>; this only wires the toggle button.
 * ------------------------------------------------------------------------- */
const THEME_ORDER = ["system", "light", "dark"];
const THEME_ICONS = { system: ICONS.system, light: ICONS.light, dark: ICONS.dark };
const THEME_LABELS = { system: "System", light: "Light", dark: "Dark" };
const themeIconEl = $("#theme-icon");
const themeLabelEl = $("#theme-label");

function getTheme() {
  const m = document.cookie.match(/(?:^|;\s*)theme=([^;]+)/);
  const t = m ? decodeURIComponent(m[1]) : "system";
  return THEME_ORDER.includes(t) ? t : "system";
}
function applyTheme(theme) {
  if (theme === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", theme);
  if (themeIconEl) themeIconEl.innerHTML = THEME_ICONS[theme];
  if (themeLabelEl) themeLabelEl.textContent = THEME_LABELS[theme];
  if (themeBtn) themeBtn.title = THEME_LABELS[theme] + " theme — click to cycle";
}
function setTheme(theme) {
  // 1-year cookie so the choice survives browser restarts.
  document.cookie = `theme=${encodeURIComponent(theme)}; path=/; max-age=31536000; samesite=lax`;
  applyTheme(theme);
}
applyTheme(getTheme());
// Swap the initial (server-fetched) favicon for the inline data-URI version
// so the browser never drops it back to a generic tile while the server is
// unreachable.
setFavicon(FAVICON_NORMAL);
if (themeBtn) {
  themeBtn.onclick = () => {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(getTheme()) + 1) % THEME_ORDER.length];
    setTheme(next);
  };
}

/* ---------------------------------------------------------------------------
 * Mobile off-canvas drawer. The CSS shows the hamburger + overlay below 768px;
 * this just wires the toggle, overlay click, Escape, and auto-close-on-select.
 * ------------------------------------------------------------------------- */
function isMobileView() { return window.matchMedia("(max-width: 768px)").matches; }
function openMenu() {
  document.body.classList.add("menu-open");
  if (menuToggle) menuToggle.setAttribute("aria-expanded", "true");
}
function closeMenu() {
  if (!document.body.classList.contains("menu-open")) return;
  document.body.classList.remove("menu-open");
  if (menuToggle) menuToggle.setAttribute("aria-expanded", "false");
}
function toggleMenu() {
  if (document.body.classList.contains("menu-open")) closeMenu();
  else openMenu();
}
if (menuToggle) menuToggle.onclick = toggleMenu;
if (menuOverlay) menuOverlay.onclick = closeMenu;
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && document.body.classList.contains("menu-open")) closeMenu();
});

loadConfig();
checkForUpdate();
loadSessions();
// Initial collapse state — before any session is opened, collapse cmd column.
document.body.classList.add("cmd-collapsed");

/* ---------------------------------------------------------------------------
 * Split divider — drag between cmd column and doc column
 * ------------------------------------------------------------------------- */
const SPLIT_KEY = "splitRatio";
function applySplitRatio(ratio) {
  // ratio is the fraction (0..1) occupied by the cmd column within #split
  const clamped = Math.min(0.85, Math.max(0.15, ratio));
  if (splitEl) splitEl.style.setProperty("--cmd-w", (clamped * 100).toFixed(3) + "%");
}
(function initSplit() {
  const stored = parseFloat(localStorage.getItem(SPLIT_KEY));
  applySplitRatio(Number.isFinite(stored) ? stored : 0.45);
})();

let splitDragging = false;
if (splitDivider) {
  splitDivider.addEventListener("mousedown", (e) => {
    splitDragging = true;
    splitDivider.classList.add("dragging");
    document.body.classList.add("dragging");
    e.preventDefault();
  });
  window.addEventListener("mousemove", (e) => {
    if (!splitDragging) return;
    const rect = (splitEl || mainEl).getBoundingClientRect();
    const x = e.clientX - rect.left;
    const ratio = x / rect.width;
    applySplitRatio(ratio);
  });
  window.addEventListener("mouseup", () => {
    if (!splitDragging) return;
    splitDragging = false;
    splitDivider.classList.remove("dragging");
    document.body.classList.remove("dragging");
    // Persist current ratio from the computed style
    const cur = getComputedStyle(splitEl || mainEl).getPropertyValue("--cmd-w").trim();
    const pct = parseFloat(cur);
    if (Number.isFinite(pct)) localStorage.setItem(SPLIT_KEY, (pct / 100).toFixed(4));
    scheduleTermFit();
  });
}

/* ---------------------------------------------------------------------------
 * Per-session terminal manager
 * ------------------------------------------------------------------------- */
const terminals = new Map(); // sessionID -> { term, fit, div, ws, running, exited, folder, claudeSession }
const metaCache = new Map(); // sessionID -> { folder, claudeSession }
let currentTermId = null;

const TermCtor = (typeof window !== "undefined") ? window.Terminal : null;
const FitCtor = (typeof window !== "undefined" && window.FitAddon) ? window.FitAddon.FitAddon : null;

function termEntry(id) { return terminals.get(id); }

function createTerminalEntry(id) {
  if (!TermCtor) return null;
  const div = document.createElement("div");
  div.className = "term-instance hidden";
  div.dataset.id = id;
  termHost.appendChild(div);

  const term = new TermCtor({
    cursorBlink: true,
    fontFamily: "Consolas, Menlo, monospace",
    fontSize: 13,
    theme: { background: "#0b0d13", foreground: "#e6e8ec" },
    scrollback: 5000,
    convertEol: false,
  });
  const fit = FitCtor ? new FitCtor() : null;
  if (fit) term.loadAddon(fit);
  term.open(div);

  // Ctrl+C copies when there's a selection, otherwise forwards ^C to the
  // PTY (the shell interrupt). Ctrl+V / Ctrl+Shift+V paste from clipboard.
  // Ctrl+Shift+C always copies (convenient when shell has ^C bound).
  // Return false from the handler to prevent xterm from also emitting
  // the keystroke to the PTY.
  term.attachCustomKeyEventHandler((ev) => {
    if (ev.type !== "keydown") return true;
    const ctrl = ev.ctrlKey && !ev.metaKey && !ev.altKey;
    if (!ctrl) return true;
    const key = ev.key.toLowerCase();
    if (key === "c") {
      if (ev.shiftKey || term.hasSelection()) {
        const sel = term.getSelection();
        if (sel) navigator.clipboard.writeText(sel).catch(() => {});
        return false;
      }
      return true; // no selection: let ^C pass through as interrupt
    }
    if (key === "v") {
      navigator.clipboard.readText().then((txt) => {
        if (!txt) return;
        if (entry && entry.ws && entry.ws.readyState === 1) {
          entry.ws.send(new TextEncoder().encode(txt));
        }
      }).catch(() => {});
      return false;
    }
    return true;
  });

  const entry = {
    term, fit, div,
    ws: null,
    running: false,
    exited: false,
    visible: false, // user explicitly revealed cmd for this session (Start/Resume/Show cmd)
    folder: "",
    claudeSession: "",
    bootCommand: null, // set when starting
  };
  terminals.set(id, entry);

  term.onData((data) => {
    if (entry.ws && entry.ws.readyState === 1) {
      entry.ws.send(new TextEncoder().encode(data));
    }
  });
  term.onResize(({ cols, rows }) => {
    if (entry.ws && entry.ws.readyState === 1) {
      try { entry.ws.send(JSON.stringify({ type: "resize", cols, rows })); } catch {}
    }
  });
  return entry;
}

function hideAllTerminals() {
  for (const [, e] of terminals) e.div.classList.add("hidden");
}

function showTerminalFor(id) {
  currentTermId = id;
  hideAllTerminals();
  let entry = termEntry(id);
  if (!entry) entry = createTerminalEntry(id);
  if (entry) {
    entry.div.classList.remove("hidden");
    if (termEmpty) termEmpty.hidden = true;
    scheduleTermFit();
  }
  // Fetch fresh meta and refresh run status. Do NOT auto-attach a WebSocket
  // or auto-expand the cmd column when a PTY is running from a prior tab —
  // the column stays collapsed until the user explicitly clicks Show cmd,
  // Start cmd, or Resume.
  refreshMeta(id).then(async () => {
    await refreshStatus(id);
    updateTermHead(id);
    updateCmdCollapse();
  });
}

async function refreshMeta(id) {
  try {
    const meta = await api(`/api/sessions/${id}/meta`);
    metaCache.set(id, {
      folder: meta.folder || "",
      claudeSession: meta.claudeSession || "",
      focus: meta.focus || "",
      metadata: meta.metadata || [],
    });
    const e = termEntry(id);
    if (e) {
      e.folder = meta.folder || "";
      e.claudeSession = meta.claudeSession || "";
    }
    if (id === currentId) {
      updateFocus(meta.focus || "");
      updateOpenCmdVisibility();
      updateMetaPanelVisibility();
    }
    return meta;
  } catch (err) {
    // Fall back to the session list fields if meta endpoint fails
    const s = sessions.find(x => x.id === id);
    const fallback = { folder: (s && s.folder) || "", claudeSession: (s && s.claudeSession) || "", focus: (s && s.focus) || "", metadata: [] };
    metaCache.set(id, fallback);
    const e = termEntry(id);
    if (e) { e.folder = fallback.folder; e.claudeSession = fallback.claudeSession; }
    return fallback;
  }
}

async function refreshStatus(id) {
  try {
    const st = await api(`/api/terminal/${id}/status`);
    const e = termEntry(id);
    if (e) {
      e.running = !!st.running;
      if (!st.running) e.exited = st.exitCode != null;
      e.folder = st.folder || e.folder || "";
      e.claudeSession = st.claudeSession || e.claudeSession || "";
    }
    return st;
  } catch {
    return null;
  }
}

function updateTermHead(id) {
  const s = sessions.find(x => x.id === id);
  const entry = termEntry(id);
  const meta = metaCache.get(id) || { folder: "", claudeSession: "" };
  const folder = (entry && entry.folder) || meta.folder || "";

  // The cmd column has no header bar anymore; the only chrome left inside
  // the column is the fallback folder form (shown only when no folder set).
  const showFolderForm = !!s && !folder;
  termFolderForm.hidden = !showFolderForm;
  if (showFolderForm) termFolderInput.value = "";
}

/* ----- Actions ----- */

async function startTerminal(id, command) {
  const entry = termEntry(id) || createTerminalEntry(id);
  if (!entry) return;
  // Size: use fit addon proposal if available, else fallback
  let cols = 80, rows = 24;
  if (entry.fit) {
    try {
      entry.fit.fit();
      cols = entry.term.cols || cols;
      rows = entry.term.rows || rows;
    } catch {}
  }
  entry.exited = false;
  entry.bootCommand = command || null;
  entry.visible = true;
  try {
    const body = { cols, rows };
    if (command) body.command = command;
    await api(`/api/terminal/${id}`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  } catch (e) {
    alert("Failed to start terminal: " + e.message);
    return;
  }
  entry.running = true;
  openTerminalSocket(id);
  updateTermHead(id);
  updateCmdCollapse();
}

function openTerminalSocket(id) {
  const entry = termEntry(id);
  if (!entry) return;
  if (entry.ws) {
    try { entry.ws.close(); } catch {}
    entry.ws = null;
  }
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/ws/terminal/${id}`);
  ws.binaryType = "arraybuffer";
  entry.ws = ws;

  ws.addEventListener("open", () => {
    // Send initial resize
    try {
      if (entry.fit) entry.fit.fit();
      const cols = entry.term.cols, rows = entry.term.rows;
      ws.send(JSON.stringify({ type: "resize", cols, rows }));
    } catch {}
  });
  ws.addEventListener("message", (ev) => {
    if (typeof ev.data === "string") {
      let msg;
      try { msg = JSON.parse(ev.data); } catch { return; }
      if (msg && msg.type === "exit") {
        entry.running = false;
        entry.exited = true;
        try { entry.term.writeln(`\r\n\x1b[90m[process exited with code ${msg.code}]\x1b[0m`); } catch {}
        if (id === currentTermId) {
          updateTermHead(id);
          updateCmdCollapse();
        }
      }
    } else {
      try { entry.term.write(new Uint8Array(ev.data)); } catch {}
    }
  });
  ws.addEventListener("close", () => {
    entry.ws = null;
    // If we thought it was running but the socket closed, re-check status
    if (entry.running) {
      refreshStatus(id).then(() => {
        if (id === currentTermId) updateTermHead(id);
      });
    }
  });
  ws.addEventListener("error", () => { /* close handler will follow */ });
}

function closeExitedTerminal(id) {
  const entry = termEntry(id);
  if (!entry) return;
  try { entry.term.dispose(); } catch {}
  try { entry.div.remove(); } catch {}
  terminals.delete(id);
  if (id === currentTermId) {
    if (termEmpty) termEmpty.hidden = false;
    updateTermHead(id);
  }
  updateCmdCollapse();
}

function startCmdForCurrent() {
  if (!currentTermId) return;
  const s = sessions.find(x => x.id === currentTermId);
  if (!s) return;
  const mdPath = s.path || "";
  const cmd = mdPath ? `claude "${freshClaudePrompt(mdPath)}"` : `claude`;
  startTerminal(currentTermId, cmd);
}

function freshClaudePrompt(statusPath) {
  // Mirrors backend freshClaudePrompt: name the embedded skill file so the
  // agent adopts the orchestrator role even if the skill isn't installed.
  if (appConfig && appConfig.skillPath) {
    return `Read and follow ${appConfig.skillPath}, then use this for status: ${statusPath}`;
  }
  return `Use this for status: ${statusPath}`;
}

function resumeClaudeForCurrent() {
  if (!currentTermId) return;
  const entry = termEntry(currentTermId);
  const meta = metaCache.get(currentTermId);
  const uuid = (entry && entry.claudeSession) || (meta && meta.claudeSession) || "";
  if (!uuid) return;
  startTerminal(currentTermId, `claude --resume ${uuid}`);
}

async function closeCmdForCurrent() {
  if (!currentTermId) return;
  const id = currentTermId;
  const entry = termEntry(id);
  // Kill the PTY server-side if running, then dispose the xterm instance
  // so the column collapses. Reopens via the header's ▶ Show cmd button.
  if (entry && entry.running) {
    try { await api(`/api/terminal/${id}`, { method: "DELETE" }); } catch (e) {
      alert("Close failed: " + e.message);
      return;
    }
  }
  closeExitedTerminal(id);
}

if (folderBrowse) {
  folderBrowse.onclick = async () => {
    try {
      const folder = await pickFolderNative();
      if (folder) folderInput.value = folder;
    } catch (e) {
      alert("Browse failed: " + e.message);
    }
  };
}
if (folderCancelBtn) {
  folderCancelBtn.onclick = () => {
    pendingFolderAction = null;
    if (folderDialog) folderDialog.close();
  };
}
if (folderDialog) {
  folderDialog.addEventListener("click", (e) => {
    if (e.target === folderDialog) {
      pendingFolderAction = null;
      folderDialog.close();
    }
  });
  folderDialog.addEventListener("cancel", () => {
    // Esc key — behave like Cancel button
    pendingFolderAction = null;
  });
}
if (folderForm) {
  folderForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!currentId) { folderDialog && folderDialog.close(); return; }
    const folder = (folderInput.value || "").trim();
    if (!folder) { folderInput.focus(); return; }
    try {
      await api(`/api/sessions/${currentId}`, {
        method: "PATCH",
        body: JSON.stringify({ folder }),
      });
      const s = sessions.find(x => x.id === currentId);
      if (s) s.folder = folder;
      await refreshMeta(currentId);
      updateTermHead(currentId);
      updateCmdCollapse();
      const action = pendingFolderAction;
      pendingFolderAction = null;
      if (folderDialog) folderDialog.close();
      if (action === "show-cmd") runShowCmd();
      else if (action === "open-cmd") runOpenCmd();
    } catch (err) {
      alert("Save folder failed: " + err.message);
    }
  });
}

if (termFolderForm) {
  termFolderForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!currentTermId) return;
    const folder = (termFolderInput.value || "").trim();
    if (!folder) { termFolderInput.focus(); return; }
    try {
      await api(`/api/sessions/${currentTermId}`, {
        method: "PATCH",
        body: JSON.stringify({ folder }),
      });
      const s = sessions.find(x => x.id === currentTermId);
      if (s) s.folder = folder;
      await refreshMeta(currentTermId);
      updateTermHead(currentTermId);
      updateCmdCollapse();
      const action = pendingFolderAction;
      pendingFolderAction = null;
      if (action === "show-cmd") runShowCmd();
      else if (action === "open-cmd") runOpenCmd();
    } catch (err) {
      alert("Save folder failed: " + err.message);
    }
  });
}

/* ----- Collapse cmd column when no xterm instance exists ----- */

function updateCmdCollapse() {
  // Rule: cmd column visible iff the user has explicitly revealed it for the
  // current session (entry.visible). Opening a session never auto-expands,
  // even if a PTY from a prior tab is still running in the backend.
  const entry = currentId ? termEntry(currentId) : null;
  const shouldCollapse = !currentId || !entry || !entry.visible;
  const was = document.body.classList.contains("cmd-collapsed");
  document.body.classList.toggle("cmd-collapsed", shouldCollapse);

  // Toggle the Show/Close cmd button label based on collapse state.
  // Hidden only when no session is selected.
  if (showCmdBtn) {
    showCmdBtn.hidden = !currentId;
    showCmdBtn.textContent = shouldCollapse ? "▶ Show cmd" : "× Close cmd";
    showCmdBtn.classList.toggle("danger", !shouldCollapse);
  }

  if (was !== shouldCollapse) scheduleTermFit();
}

let pendingFolderAction = null;

function currentFolder() {
  if (!currentId) return "";
  const entry = termEntry(currentId);
  const meta = metaCache.get(currentId);
  const s = sessions.find(x => x.id === currentId);
  return (entry && entry.folder) || (meta && meta.folder) || (s && s.folder) || "";
}

function promptForFolder(action) {
  if (!currentId || !folderDialog) return;
  pendingFolderAction = action;
  if (folderInput) folderInput.value = "";
  try { folderDialog.showModal(); } catch { folderDialog.show(); }
  try { folderInput && folderInput.focus(); } catch {}
}

function runShowCmd() {
  if (!currentId) return;
  const e = termEntry(currentId) || createTerminalEntry(currentId);
  if (!e) return;
  e.visible = true;
  e.div.classList.remove("hidden");
  if (termEmpty) termEmpty.hidden = true;
  if (e.running && !e.ws) {
    openTerminalSocket(currentId);
    updateTermHead(currentId);
    updateCmdCollapse();
    return;
  }
  const meta = metaCache.get(currentId);
  const uuid = (e.claudeSession) || (meta && meta.claudeSession) || "";
  if (uuid) resumeClaudeForCurrent();
  else startCmdForCurrent();
}

async function runOpenCmd() {
  if (!currentId) return;
  try {
    await api(`/api/sessions/${currentId}/open-cmd`, { method: "POST" });
  } catch (e) {
    alert("Open cmd failed: " + e.message);
  }
}

if (showCmdBtn) {
  showCmdBtn.onclick = () => {
    if (!currentId) return;
    // Toggle: if the cmd column is currently visible, close it. Otherwise
    // fall through to the existing show flow (prompting for folder first
    // when unset).
    const collapsed = document.body.classList.contains("cmd-collapsed");
    if (!collapsed) { closeCmdForCurrent(); return; }
    if (!currentFolder()) { promptForFolder("show-cmd"); return; }
    runShowCmd();
  };
}

if (openFileBtn) {
  openFileBtn.onclick = async () => {
    if (!currentId) return;
    try {
      await api(`/api/sessions/${currentId}/open`, { method: "POST" });
    } catch (e) {
      alert("Open failed: " + e.message);
    }
  };
}

if (openCmdBtn) {
  openCmdBtn.onclick = () => {
    if (!currentId) return;
    if (!currentFolder()) { promptForFolder("open-cmd"); return; }
    runOpenCmd();
  };
}

function updateFocus(text) {
  if (!viewFocus) return;
  const t = (text || "").trim();
  viewFocus.textContent = t;
  viewFocus.hidden = !t;
}

function updateOpenCmdVisibility() {
  if (!openCmdBtn) return;
  openCmdBtn.hidden = !currentId;
}

const META_SHOWN_KEY = "meta_panel_shown";
let metaShown = localStorage.getItem(META_SHOWN_KEY) === "1";

function renderMetaPanel(meta) {
  if (!metaPanel) return;
  metaPanel.innerHTML = "";
  if (!meta || !meta.length) {
    const n = document.createElement("div");
    n.className = "v";
    n.textContent = "(no frontmatter)";
    n.style.gridColumn = "1 / -1";
    metaPanel.appendChild(n);
    return;
  }
  for (const row of meta) {
    const k = document.createElement("div");
    k.className = "k";
    k.textContent = row.key;
    const v = document.createElement("div");
    v.className = "v";
    v.textContent = row.value;
    metaPanel.appendChild(k);
    metaPanel.appendChild(v);
  }
}

function updateMetaPanelVisibility() {
  if (!metaPanel || !metaToggleBtn) return;
  metaPanel.hidden = !metaShown || !currentId;
  metaToggleBtn.setAttribute("aria-pressed", metaShown ? "true" : "false");
  if (metaShown && currentId) {
    const meta = metaCache.get(currentId);
    renderMetaPanel(meta && meta.metadata);
  }
}

if (metaToggleBtn) {
  metaToggleBtn.onclick = () => {
    metaShown = !metaShown;
    localStorage.setItem(META_SHOWN_KEY, metaShown ? "1" : "0");
    updateMetaPanelVisibility();
  };
}

/* ----- Resize / fit ----- */

let fitTimer = null;
function scheduleTermFit() {
  if (fitTimer) clearTimeout(fitTimer);
  fitTimer = setTimeout(() => {
    fitTimer = null;
    const entry = currentTermId ? termEntry(currentTermId) : null;
    if (entry && entry.fit && !entry.div.classList.contains("hidden")) {
      try { entry.fit.fit(); } catch {}
    }
  }, 100);
}
window.addEventListener("resize", scheduleTermFit);

window.addEventListener("beforeunload", () => {
  for (const [, e] of terminals) {
    try { if (e.ws) e.ws.close(); } catch {}
    try { e.term.dispose(); } catch {}
  }
  terminals.clear();
});
