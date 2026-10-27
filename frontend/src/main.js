import './style.css';
import '@xterm/xterm/css/xterm.css';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';

import * as App from '../wailsjs/go/main/App';
import { EventsOn, EventsOff, BrowserOpenURL } from '../wailsjs/runtime/runtime';
// The DOM-free helpers live apart so `node --test` in frontend/test can cover
// them without a browser, xterm or the Wails bindings.
import { DEFAULT_PORT, fmtSize, osc7Path, parentPath, parsePort, stripPrompt } from './utils.js';

const state = {
  view: 'session',
  sessions: [],
  profiles: [],
  tunnels: [],
  currentSid: '',
  selectedProfile: null,
  term: null,
  fit: null,
  ptyUnsub: null,
  editingTunnel: '',
  pendingNew: false,
  cwds: {},
  prevCwds: {},
  // retryConnect is the attempt a TOFU prompt interrupted: a thunk that dials
  // again (plain Connect or duplicate-terminal) once the key is trusted.
  retryConnect: null,
};
const authMode = { active: false, echo: false, buffer: '' };
// lineBuf accumulates what is typed at the shell prompt so a submitted line can
// be inspected for "cd". Tab-completed text never reaches us as input, so a line
// containing a tab is flagged and ignored rather than resolved to a prefix.
const lineBuf = { text: '', tab: false };
const FOLLOW_KEY = 'gravel.followTerminalFolder';
const HIDDEN_KEY = 'gravel.showHidden';
const THEME_KEY = 'gravel.theme';

function $(s) { return document.querySelector(s); }
function cwd() { return state.cwds[state.currentSid] || '/'; }
// Feedback no longer flashes in a bottom status bar: every message goes to the
// backend log, so the Logs tab has a single source for its lines and Dump
// exports exactly what the user saw there.
function msg(t) { try { App.LogUI(t); } catch (e) {} }
// writeNotice paints a one-off status line into the terminal. Connection
// failures arrive after the running session has been repainted, so writing
// here - rather than during the attempt - is what keeps the reason on screen
// instead of wiping it with that repaint.
function writeNotice(text) {
  if (!state.term) return;
  try { state.term.write('\r\n\x1b[2m' + text + '\x1b[0m\r\n'); } catch (e) {}
}
function followOn() { const el = $('#followTerm'); return !!(el && el.checked); }
function showHiddenOn() { const el = $('#showHidden'); return !!(el && el.checked); }

// ---------- view switching ----------
function switchView(name) {
  state.view = name;
  document.querySelectorAll('.navitem').forEach((b) => b.classList.toggle('active', b.dataset.view === name));
  document.querySelectorAll('.view').forEach((v) => v.classList.toggle('active', v.id === 'view-' + name));
  if (name === 'terminal') {
    ensureTerminal();
    loadFiles(cwd());
    startQueuePoll();
    requestAnimationFrame(() => { refitTerminal(); try { state.term.focus(); } catch (e) {} });
  } else { stopQueuePoll(); }
  if (name === 'tunnels') refreshTunnels();
  if (name === 'settings') refreshSettings();
  if (name === 'logs') refreshLogs();
  if (name === 'session') { refreshProfiles(); refreshSessions(); }
}

// ---------- terminal ----------
// Bounded font size, set from the Settings tab (no wheel/keyboard zoom: a
// resize the remote has not acknowledged garbles its line wrapping - the
// Settings change always pushes the new size through refitTerminal).
const ZOOM_KEY = 'gravel.termFontSize';
const FONT_MIN = 10, FONT_MAX = 28, FONT_DEFAULT = 14;

function termFontSize() {
  const saved = parseInt(localStorage.getItem(ZOOM_KEY) || '', 10);
  return saved >= FONT_MIN && saved <= FONT_MAX ? saved : FONT_DEFAULT;
}

function setTermFont(px) {
  const size = Math.min(FONT_MAX, Math.max(FONT_MIN, Math.round(px)));
  if (state.term) { try { state.term.options.fontSize = size; } catch (e) {} }
  refitTerminal();
  try { localStorage.setItem(ZOOM_KEY, String(size)); } catch (e) {}
  return size;
}

// The palette lives in CSS custom properties, so the terminal and the rest of
// the UI can never disagree about which theme is active.
function termTheme() {
  const css = getComputedStyle(document.documentElement);
  const v = (name, fallback) => (css.getPropertyValue(name) || fallback).trim();
  return { background: v('--bg', '#1e1e1e'), foreground: v('--fg', '#e0e0e0'), cursor: v('--accent', '#7c8cff') };
}

function ensureTerminal() {
  if (state.term) return;
  state.term = new Terminal({ cursorBlink: true, fontFamily: 'Consolas, monospace', fontSize: termFontSize(), theme: termTheme() });
  state.fit = new FitAddon();
  state.term.loadAddon(state.fit);
  state.term.open($('#term'));
  state.term.onData((d) => {
    if (authMode.active) { handleAuthKey(d); return; }
    if (state.currentSid) { feedLine(d); App.WriteInput(state.currentSid, d).catch(() => {}); }
  });
  state.term.onResize((s) => { if (state.currentSid) App.ResizeSession(state.currentSid, s.cols, s.rows); });
  // PuTTY mouse model: finishing a selection copies it to the clipboard, and
  // right-click pastes into the live session. Ctrl+V keeps working through
  // xterm's own paste handling. The native webview menu is suppressed here so
  // the gesture has a single, predictable meaning.
  state.term.onSelectionChange(() => {
    try {
      const sel = state.term.getSelection();
      if (sel) navigator.clipboard.writeText(sel).catch(() => {});
    } catch (e) {}
  });
  const termHost = $('#term');
  if (termHost) termHost.addEventListener('contextmenu', async (e) => {
    e.preventDefault();
    // While a login prompt owns the keyboard, pasting into the PTY would
    // bypass it; refuse rather than misroute the clipboard.
    if (authMode.active) { msg('answer the login prompt first'); return; }
    if (!state.currentSid) { msg('connect first'); return; }
    try {
      const text = await navigator.clipboard.readText();
      if (text) App.WriteInput(state.currentSid, text).catch(() => {});
    } catch (err) {
      msg('right-click paste unavailable - use Ctrl+V');
    }
  });
  // OSC 7 ("file://host/path") is emitted by prompt integrations some distros
  // ship and reports the real cwd, so it wins over our own reading of the
  // typed line. We only listen - nothing is injected into the remote shell.
  try {
    state.term.parser.registerOscHandler(7, (data) => {
      const p = osc7Path(data);
      if (p && followOn()) moveTo(state.currentSid, p);
      return true;
    });
  } catch (e) { /* xterm build without parser access: cd tracking still works */ }
  setTimeout(() => { refitTerminal(); }, 60);
  window.addEventListener('resize', () => { if (state.view === 'terminal') refitTerminal(); });
}

// ---------- follow the terminal's folder ----------
function resetLine() { lineBuf.text = ''; lineBuf.tab = false; }

function feedLine(d) {
  for (const ch of d) {
    const code = ch.charCodeAt(0);
    if (ch === '\r' || ch === '\n') { submitLine(); }
    else if (ch === '\x7f' || code === 8) { lineBuf.text = lineBuf.text.slice(0, -1); }
    else if (code === 3 || code === 21) { resetLine(); } // ^C and ^U abandon the line
    else if (ch === '\t') { lineBuf.tab = true; }
    else if (code >= 32) { lineBuf.text += ch; }
  }
}

// shellLineFromScreen reads back the line the shell actually accepted, from the
// terminal's own buffer. History recall and tab completion are written to the
// screen by the remote shell and never pass through our input stream, so the
// typed buffer alone cannot be trusted for those.
function shellLineFromScreen() {
  if (!state.term) return '';
  try {
    const b = state.term.buffer.active;
    const row = b.getLine(b.baseY + b.cursorY);
    if (!row) return '';
    const text = row.translateToString(true);
    // Everything after the last prompt terminator is the command itself.
    return stripPrompt(text);
  } catch (e) { return ''; }
}

async function submitLine() {
  const sid = state.currentSid;
  const typed = lineBuf.text;
  const unreliable = lineBuf.tab;
  resetLine();
  if (!followOn() || !sid) return;
  // An empty buffer means the line came from history; the tab flag means it was
  // completed. Both cases have to be read back off the screen.
  const line = (unreliable || !typed.trim()) ? shellLineFromScreen() : typed;
  if (!line.trim()) return;
  let next = '';
  try { next = await App.FollowCwd(sid, line); } catch (e) { return; }
  // "cd -" is the one case only our own history can answer.
  if (!next && /^\s*(cd|pushd)\s+-\s*$/.test(line) && state.prevCwds[sid]) next = state.prevCwds[sid];
  await moveTo(sid, next);
}

// moveTo re-points the browser pane, remembering where it was for "cd -". The
// session is passed in because the IPC round trip may outlive the user's click
// on another tab; the pane only ever follows the session that is on screen.
async function moveTo(sid, next) {
  if (!sid || sid !== state.currentSid) return;
  const cur = state.cwds[sid] || '/';
  if (!next || next === cur) return;
  state.prevCwds[sid] = cur;
  await loadFiles(next);
  msg('following: ' + next);
}

// ---------- in-terminal auth prompt (PuTTY-style) ----------
EventsOn('auth:prompt', (p) => {
  switchView('terminal');
  ensureTerminal();
  resetLine();
  authMode.active = true; authMode.echo = !!p.echo; authMode.buffer = '';
  // The question carries no leading newline: the Enter that finished the
  // previous answer already moved down a line (see handleAuthKey), and a fresh
  // terminal starts at the top. Adding one here was the blank gap PuTTY does
  // not show between "login as:" and the password prompt.
  state.term.write(p.question + ' ');
  state.term.focus();
});
// Authentication status lines (a rejected password, giving up) belong in the
// terminal where the user is typing, not only in the bottom status line. The
// line owns its trailing newline so the prompt that follows starts fresh; it
// also claims a fresh row when a prompt is mid-line, so a FATAL ERROR can never
// append itself behind a waiting "login as: ".
EventsOn('auth:notice', (t) => {
  if (!state.term) return;
  try {
    if (authMode.active) { state.term.write('\r\n'); authMode.active = false; authMode.buffer = ''; }
    state.term.write(t + '\r\n');
  } catch (e) {}
});
// A newer Connect attempt superseded a waiting prompt: the backend answered it
// with a cancellation, so drop the input mode here too.
EventsOn('auth:cancel', () => {
  if (state.term && authMode.active) { try { state.term.write('\r\n'); } catch (e) {} }
  authMode.active = false; authMode.echo = false; authMode.buffer = '';
});

// The remote shell exited ("exit"/logout) or the link dropped. The backend has
// already torn the session down, so drop the tab and move on.
EventsOn('session:exit', (sid) => {
  const gone = state.sessions.find((s) => s.id === sid);
  msg(gone ? 'session ended: ' + gone.name : 'session ended');
  afterSessionGone(sid).catch(() => {});
});

// afterSessionGone handles a tab disappearing, by logout or by the user
// closing it. With nothing left to show it clears the dead screen and returns
// to where a new connection starts - PuTTY closes its window on logout, and a
// blank terminal with a blinking cursor just reads as hung.
async function afterSessionGone(sid) {
  const wasCurrent = !state.currentSid || state.currentSid === sid;
  resetLine();
  await refreshSessions();
  if (!wasCurrent || !state.term) return;
  if (state.currentSid) {
    // Another session takes over: start clean, exactly like clicking its tab.
    try { state.term.reset(); } catch (e) {}
    await loadFiles(cwd());
    return;
  }
  try {
    state.term.reset();
    state.term.write('\x1b[2m[disconnected]\x1b[0m\r\n');
  } catch (e) {}
  switchView('session');
}

function handleAuthKey(d) {
  for (const ch of d) {
    const code = ch.charCodeAt(0);
    if (ch === '\r' || ch === '\n') { state.term.write('\r\n'); const a = authMode.buffer; authMode.active = false; authMode.buffer = ''; App.AuthAnswer(a); return; }
    else if (ch === '\x7f' || code === 8) { if (authMode.buffer.length > 0) { authMode.buffer = authMode.buffer.slice(0, -1); if (authMode.echo) state.term.write('\b \b'); } }
    else if (code === 3) { authMode.active = false; authMode.buffer = ''; state.term.write('^C\r\n'); App.AuthAnswer(''); return; }
    else if (code >= 32) { authMode.buffer += ch; if (authMode.echo) state.term.write(ch); }
  }
}

// ---------- sessions / tabs ----------
async function refreshSessions() {
  try { state.sessions = await App.ListSessions(); }
  catch (e) { msg('sessions: ' + e); return; }
  // Drop the pane state of sessions that no longer exist so the maps cannot
  // grow without bound across reconnects.
  const live = new Set(state.sessions.map((s) => s.id));
  Object.keys(state.cwds).forEach((id) => {
    if (!live.has(id)) { delete state.cwds[id]; delete state.prevCwds[id]; }
  });
  // Adopt the backend's browse path for every session the pane has not seen
  // yet. A fresh session starts at the remote user's home directory - which is
  // where the shell itself landed - rather than at "/".
  let fresh = false;
  state.sessions.forEach((s) => {
    if (!(s.id in state.cwds)) { state.cwds[s.id] = s.filesPath || '/'; fresh = true; }
  });
  if (!state.currentSid || !state.sessions.find((s) => s.id === state.currentSid)) {
    state.currentSid = state.sessions.length ? state.sessions[state.sessions.length - 1].id : '';
  }
  renderTabs();
  renderSessionList();
  // While a new session is still dialling or logging in, the shared screen
  // belongs to its prompts: re-attaching the previous session here - which the
  // 5s poll does constantly - would stream its output over the "login as:".
  if (state.pendingNew) {
    if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  } else if (state.currentSid) attachPty(state.currentSid);
  // With no session left, release the last PTY subscription; otherwise every
  // reconnect leaves another dead listener behind.
  else if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  // Nothing has been rendered for a session that just appeared, so paint it now;
  // otherwise the pane keeps showing the pre-connect listing.
  if (fresh && state.currentSid && state.view === 'terminal') loadFiles(cwd());
}

function attachPty(sid) {
  if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  EventsOff('pty:' + sid);
  state.ptyUnsub = EventsOn('pty:' + sid, (data) => { if (state.term) { try { state.term.write(data); } catch (e) {} } });
  // Push the widget's real size to this session now that its output is on
  // screen. onResize alone misses resizes that happened while no session was
  // current (zoom during login), and a remote still at its 100x30 default
  // hard-wraps at the wrong column - the garbled output this prevents.
  if (state.term) App.ResizeSession(sid, state.term.cols, state.term.rows);
}

function renderTabs() {
  const tb = $('#tabs'); if (!tb) return; tb.textContent = '';
  // While a new session is still authenticating, its placeholder tab owns the
  // active highlight so the old tab does not also look selected.
  const activeId = state.pendingNew ? '' : state.currentSid;
  state.sessions.forEach((s) => {
    const t = document.createElement('div'); t.className = 'tab' + (s.id === activeId ? ' active' : '');
    const dot = document.createElement('span'); dot.className = 'dot ' + s.state;
    const lbl = document.createElement('span'); lbl.textContent = s.name || s.id;
    const x = document.createElement('button'); x.className = 'close'; x.textContent = '\u00d7';
    x.addEventListener('click', (ev) => { ev.stopPropagation(); confirmCloseSession(s.id); });
    t.appendChild(dot); t.appendChild(lbl); t.appendChild(x);
    t.addEventListener('click', () => selectSession(s.id));
    t.addEventListener('contextmenu', (ev) => { ev.preventDefault(); showTabMenu(ev.clientX, ev.clientY, s.id); });
    tb.appendChild(t);
  });
  // Placeholder tab shown the instant "Duplicate" is pressed, before the
  // backend has finished dialling and authenticating.
  if (state.pendingNew) {
    const p = document.createElement('div'); p.className = 'tab active pending';
    const dot = document.createElement('span'); dot.className = 'dot connecting';
    const lbl = document.createElement('span'); lbl.textContent = 'New session\u2026';
    p.appendChild(dot); p.appendChild(lbl);
    tb.appendChild(p);
  }
  // No trailing "+": a new connection is started from the Sessions view, and
  // a second terminal to the same server stays under the tab's right-click
  // "Duplicate terminal".
}

// newTerminalLike opens a second session to the server an existing tab points
// at, so the duplicate flow reuses the connection target rather than the form.
async function newTerminalLike(sid) {
  const target = sid || state.currentSid;
  if (!target) { switchView('session'); return; }
  switchView('terminal');
  ensureTerminal();
  // Open the tab immediately (placeholder) and clear the shared screen so the
  // login prompt lands in the new tab rather than the current session's view.
  state.pendingNew = true;
  try { state.term.reset(); } catch (e) {}
  // Detach the live session while the duplicate logs in, so its output cannot
  // stream over the login prompts (see doConnect for the same reasoning).
  if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  renderTabs();
  let res;
  try { res = await App.NewSessionLike(target); }
  catch (e) { msg('new terminal: ' + e); state.pendingNew = false; renderTabs(); await repaintCurrent(); return; }
  state.pendingNew = false;
  // The same hand-back order as doConnect: repaint the session that survived
  // first, then report the reason through the shared handler, so a failed
  // duplicate is neither silent nor missing its host-key dialog.
  if (!res || !res.sid) await repaintCurrent();
  await handleConnectResult(res, () => App.NewSessionLike(target));
}

// showTabMenu is a lightweight custom context menu (the webview has no native
// one), dismissed by the next click anywhere.
function showTabMenu(x, y, sid) {
  hideTabMenu();
  const menu = document.createElement('div'); menu.id = 'tabMenu'; menu.className = 'ctxMenu';
  const item = (label, fn) => {
    const d = document.createElement('div'); d.className = 'ctxItem'; d.textContent = label;
    d.addEventListener('click', () => { hideTabMenu(); fn(); });
    return d;
  };
  menu.appendChild(item('Duplicate terminal', () => newTerminalLike(sid)));
  menu.appendChild(item('Close terminal', () => confirmCloseSession(sid)));
  menu.style.left = '0px'; menu.style.top = '0px';
  document.body.appendChild(menu);
  // Then slide it back inside the window. A right-click near the right or bottom
  // edge used to hang part of the menu - and its buttons - off screen, with no
  // native menu to shrink it for us.
  const box = menu.getBoundingClientRect();
  const left = Math.max(4, Math.min(x, window.innerWidth - box.width - 4));
  const top = Math.max(4, Math.min(y, window.innerHeight - box.height - 4));
  menu.style.left = left + 'px'; menu.style.top = top + 'px';
  // Defer so the opening right-click does not immediately trigger the close.
  setTimeout(() => document.addEventListener('click', hideTabMenu, { once: true }), 0);
}

function hideTabMenu() { const m = document.getElementById('tabMenu'); if (m) m.remove(); }

function renderSessionList() {
  const ul = $('#sessionList'); if (!ul) return; ul.textContent = '';
  if (!state.sessions.length) { const li = document.createElement('li'); li.textContent = 'No active sessions'; ul.appendChild(li); }
  state.sessions.forEach((s) => {
    const li = document.createElement('li'); if (s.id === state.currentSid) li.classList.add('selected');
    const dot = document.createElement('span'); dot.className = 'dot ' + s.state;
    const nm = document.createElement('span'); nm.className = 'nm'; nm.textContent = s.name;
    const sub = document.createElement('span'); sub.className = 'sub'; sub.textContent = s.host;
    li.appendChild(dot); li.appendChild(nm); li.appendChild(sub);
    li.addEventListener('click', () => { selectSession(s.id); switchView('terminal'); });
    ul.appendChild(li);
  });
}

function selectSession(sid) {
  if (!sid || sid === state.currentSid) return;
  state.currentSid = sid;
  resetLine();
  renderTabs(); renderSessionList();
  // Repaint the session's saved screen: the terminal widget is shared between
  // tabs, so a bare re-subscribe would leave the view blank until the remote
  // happened to send something new.
  redrawSession(sid);
  if (state.view === 'terminal') loadFiles(cwd());
}

// redrawSession swaps the shared terminal to another session in a strict
// order: detach live output, fetch the screen snapshot, reset, paint it, and
// only then re-attach. Subscribing before the snapshot lands let fresh PTY
// bytes interleave with the repaint - the garbled, half-scrolled screens
// (clipped first characters) this replaces.
//
// redrawSeq makes an in-flight snapshot expire when the user switches again:
// two overlapping SessionScreen calls can resolve out of order, and painting the
// older screen last would leave the wrong session on display - with the newer
// one's output already streaming into it - until the next redraw.
let redrawSeq = 0;

function redrawSession(sid) {
  const mine = ++redrawSeq;
  if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  return App.SessionScreen(sid).then((text) => {
    if (!state.term || mine !== redrawSeq) return;
    try { state.term.reset(); } catch (e) {}
    if (text) { try { state.term.write(text + '\r\n'); } catch (e) {} }
    if (state.currentSid === sid) attachPty(sid);
  }).catch(() => { if (mine === redrawSeq && state.currentSid === sid) attachPty(sid); });
}

// repaintCurrent re-attaches and redraws whichever session is current after a
// connection attempt left the shared screen showing the login flow instead. It
// returns the redraw promise so a caller can write its message afterwards.
function repaintCurrent() {
  if (!state.currentSid) { renderTabs(); return Promise.resolve(); }
  return redrawSession(state.currentSid);
}

async function closeSession(sid) {
  await App.CloseSession(sid);
  await afterSessionGone(sid);
}

// confirmCloseSession guards the x on a tab and the context-menu close: a live
// shell may hold running work, so ask before tearing it down.
function confirmCloseSession(sid) {
  const s = state.sessions.find((x) => x.id === sid);
  const name = s ? (s.name || s.id) : sid;
  if (!confirm('close ' + name + '?')) return;
  closeSession(sid);
}

// ---------- connect ----------
function readForm() {
  return {
    name: $('#svName').value.trim(),
    host: $('#svHost').value.trim(),
    // null means the field is not a usable port: blank still means 22, but
    // "abc" or "70000" is refused at connect/save instead of reaching the
    // backend as NaN (which arrives over IPC as 0).
    port: parsePort($('#svPort').value, DEFAULT_PORT),
    auth: $('#svAuth').value,
    keyPath: $('#svKey').value.trim(),
  };
}

// connectWith starts a session; the backend always asks for the login name
// (and password or key passphrase) in the terminal.
function connectWith(f) {
  return f.auth === 'key' ? App.ConnectKey(f.host, f.port, f.keyPath) : App.Connect(f.host, f.port);
}

async function doConnect() {
  const f = readForm();
  if (!f.host) { msg('host required'); return; }
  if (f.port === null) { msg('port must be 1-65535'); return; }
  // A server that already has a live terminal is opened by switching to it,
  // not by dialling a second session to the same place - that is what the
  // tab's right-click "Duplicate terminal" is for.
  const addr = f.host + ':' + f.port;
  const open = state.sessions.find((s) => s.host === addr);
  if (open) { selectSession(open.id); switchView('terminal'); msg('already connected: ' + (open.name || addr)); return; }
  switchView('terminal');
  ensureTerminal();
  // Fresh canvas for every new connection, the way PuTTY opens a new window -
  // even while another session is live. Its output is detached during the
  // login so it cannot interleave with the new "login as:", and switching
  // back to its tab repaints it from the backend screen.
  state.pendingNew = true;
  try { state.term.reset(); } catch (e) {}
  if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  renderTabs();
  let res;
  try { res = await connectWith(f); }
  catch (e) { state.pendingNew = false; msg('connect error: ' + e); renderTabs(); repaintCurrent(); return; }
  state.pendingNew = false;
  // A failed or cancelled attempt leaves its login text on screen. Hand the
  // view back to the session that is still running first, then write the
  // reason under it - in the other order the repaint erases the message.
  if (!res || !res.sid) await repaintCurrent();
  await handleConnectResult(res, () => connectWith(f));
}

async function handleConnectResult(res, retry) {
  if (!res) return;
  if (res.kind === 'unknown_host') { showTOFU(res, retry); return; }
  if (res.error) {
    msg(res.error);
    // "cancelled" is this attempt being dropped for a newer one; it is not
    // something to splash across the session the user is working in.
    if (res.kind !== 'cancelled') writeNotice(res.error);
    return;
  }
  if (res.sid) { state.currentSid = res.sid; await refreshSessions(); }
}

// showTOFU asks the user to trust a first-seen host key. retry re-runs exactly
// the attempt that was rejected, so accepting resumes that flow - including a
// duplicate terminal, which has nothing to do with the Sessions form.
function showTOFU(info, retry) {
  state.retryConnect = typeof retry === 'function' ? retry : null;
  $('#tofu_addr').textContent = info.addr;
  $('#tofu_fp').textContent = info.fingerprint;
  $('#tofu_kt').textContent = 'key type: ' + (info.keyType || '?');
  $('#tofu').showModal();
}

async function acceptTOFU() {
  const info = { addr: $('#tofu_addr').textContent, fingerprint: $('#tofu_fp').textContent };
  try { await App.Trust(info.addr, info.fingerprint); } catch (e) { msg('trust failed: ' + e); return; }
  $('#tofu').close();
  const retry = state.retryConnect;
  state.retryConnect = null;
  if (!retry) { msg('host key trusted'); return; }
  switchView('terminal'); ensureTerminal();
  // Accepting the key starts a second full login on the shared screen, so it
  // takes the same path as doConnect: placeholder tab, cleared canvas and the
  // running session detached until the prompts are through.
  state.pendingNew = true;
  try { state.term.reset(); } catch (e) {}
  if (state.ptyUnsub) { try { state.ptyUnsub(); } catch (e) {} state.ptyUnsub = null; }
  renderTabs();
  let res;
  try { res = await retry(); }
  catch (e) { state.pendingNew = false; msg('retry error: ' + e); renderTabs(); repaintCurrent(); return; }
  state.pendingNew = false;
  if (!res || !res.sid) await repaintCurrent();
  await handleConnectResult(res, retry);
}

// ---------- saved servers ----------
// dragPID holds the profile id currently under a drag-and-drop reorder; the
// new sequence is persisted through ReorderProfiles so it survives restarts.
let dragPID = '';

async function refreshProfiles() {
  try { state.profiles = await App.ListProfiles(); }
  catch (e) { msg('profiles: ' + e); return; }
  const ul = $('#serverList'); if (!ul) return; ul.textContent = '';
  if (!state.profiles.length) { const li = document.createElement('li'); li.textContent = 'No saved servers'; ul.appendChild(li); return; }
  state.profiles.forEach((p) => {
    const li = document.createElement('li');
    if (state.selectedProfile && state.selectedProfile.id === p.id) li.classList.add('selected');
    const nm = document.createElement('span'); nm.className = 'nm'; nm.textContent = p.name;
    const sub = document.createElement('span'); sub.className = 'sub'; sub.textContent = p.host + ':' + p.port;
    li.appendChild(nm); li.appendChild(sub);
    li.draggable = true;
    li.addEventListener('dragstart', (ev) => {
      dragPID = p.id; li.classList.add('dragging');
      ev.dataTransfer.effectAllowed = 'move';
      try { ev.dataTransfer.setData('text/plain', p.id); } catch (e) {}
    });
    li.addEventListener('dragend', () => {
      // Only clean up here: a completed drop already triggered its own
      // refresh, and a second ListProfiles could land after the reorder was
      // persisted and briefly show the pre-drag order again.
      dragPID = '';
      li.classList.remove('dragging');
    });
    li.addEventListener('dragover', (ev) => {
      if (!dragPID || dragPID === p.id) return;
      ev.preventDefault(); ev.dataTransfer.dropEffect = 'move'; li.classList.add('dropon');
    });
    li.addEventListener('dragleave', () => li.classList.remove('dropon'));
    li.addEventListener('drop', (ev) => {
      ev.preventDefault(); li.classList.remove('dropon');
      if (!dragPID || dragPID === p.id) return;
      // Insert before or after the target depending on which half was hit.
      const ids = state.profiles.map((x) => x.id).filter((id) => id !== dragPID);
      const r = li.getBoundingClientRect();
      let to = ids.indexOf(p.id);
      if (to < 0) to = ids.length;
      if (ev.clientY > r.top + r.height / 2) to++;
      ids.splice(to, 0, dragPID);
      dragPID = '';
      App.ReorderProfiles(ids).then(refreshProfiles).catch((e) => { msg('reorder: ' + e); refreshProfiles(); });
    });
    li.addEventListener('click', () => { state.selectedProfile = p; refreshProfiles(); });
    li.addEventListener('dblclick', () => { state.selectedProfile = p; fillForm(p); doConnect(); });
    ul.appendChild(li);
  });
}

function fillForm(p) {
  $('#svName').value = p.name || '';
  $('#svHost').value = p.host || '';
  $('#svPort').value = p.port || 22;
  $('#svAuth').value = p.auth || 'password';
  $('#svKey').value = p.keyPath || '';
  $('#svKeyWrap').hidden = p.auth !== 'key';
}

function saveServer() {
  const f = readForm();
  if (!f.host) { msg('host required'); return; }
  if (f.port === null) { msg('port must be 1-65535'); return; }
  App.SaveProfile(f.name, f.host, f.port, f.auth, f.keyPath).then(() => { msg('saved'); refreshProfiles(); }).catch((e) => msg('save: ' + e));
}

function deleteServer() {
  if (!state.selectedProfile) { msg('select a saved server first'); return; }
  // No confirm dialog: Delete is an explicit click on a selected row, and the
  // browser-style confirm() popup breaks the native feel of the app.
  const name = state.selectedProfile.name;
  App.DeleteProfile(state.selectedProfile.id).then(() => { state.selectedProfile = null; refreshProfiles(); msg('deleted ' + name); }).catch((e) => msg('delete: ' + e));
}

// importHosts opens the paste-a-list dialog; the backend parses each line and
// PutNamed keeps re-imports from duplicating existing names.
function openImportDialog() {
  const ta = $('#importText'); if (ta) ta.value = '';
  $('#importDlg').showModal();
}

function runImport() {
  const text = $('#importText').value;
  if (!text.trim()) { msg('nothing to import'); return; }
  App.ImportHosts(text).then((n) => {
    $('#importDlg').close();
    msg(n + ' host(s) imported');
    refreshProfiles();
  }).catch((e) => msg('import: ' + e));
}

// pickImportFile loads a hosts file into the preview box, replacing whatever
// was pasted there; nothing is saved until Import is pressed.
function pickImportFile() {
  App.PickHostFile().then((text) => {
    if (!text) return;
    $('#importText').value = text;
  }).catch((e) => msg('import file: ' + e));
}

// importFromPutty pulls the session list PuTTY keeps in the registry into
// saved sessions. Writes immediately, deduped by name; this is the only
// PuTTY import path - nothing happens automatically at startup.
function importFromPutty() {
  App.ImportPuttySessions().then((n) => {
    msg(n + ' PuTTY session(s) imported');
    refreshProfiles();
  }).catch((e) => msg('putty import: ' + e));
}

// ---------- files (browser pane docked left of the terminal) ----------
// listSeq numbers every listing request. Two of them can overlap for the same
// session (a row click plus a follow-the-terminal move), and the one that
// resolves last would otherwise paint stale rows over fresh ones and leave
// state.cwds pointing at the directory that is no longer on screen.
let listSeq = 0;

async function loadFiles(p) {
  // The session is captured up front: the IPC round trip can outlive a tab
  // switch, and painting session A's directory under session B would both
  // mislabel the pane and aim every row button at the wrong host.
  const sid = state.currentSid;
  const seq = ++listSeq;
  if (!sid) { const tb = $('#tbody'); if (tb) tb.textContent = ''; $('#pathbar').textContent = '/ (no session)'; return; }
  try {
    const entries = await App.ListFiles(sid, p);
    if (sid !== state.currentSid || seq !== listSeq) return;
    state.cwds[sid] = p; $('#pathbar').textContent = p;
    const tb = $('#tbody');
    const frag = document.createDocumentFragment();
    (entries || []).forEach((en) => {
      // Dotfiles stay out of the list until "Show hidden" is ticked.
      if (!showHiddenOn() && en.name.charAt(0) === '.' && en.name !== '.' && en.name !== '..') return;
      const tr = document.createElement('tr');
      const nm = document.createElement('td'); nm.className = 'name'; nm.textContent = (en.isDir ? '[dir] ' : '') + en.name;
      nm.title = en.path; // the narrow pane ellipsises long names
      nm.addEventListener('click', () => { if (en.isDir) loadFiles(en.path); });
      const sz = document.createElement('td'); sz.className = 'sz'; sz.textContent = en.isDir ? '-' : fmtSize(en.size);
      const cell = (cls, text) => { const td = document.createElement('td'); td.className = cls; td.textContent = text; return td; };
      const ac = document.createElement('td'); ac.className = 'ops';
      const dl = document.createElement('button'); dl.textContent = 'download';
      dl.title = en.isDir ? 'download ' + en.name + ' and its contents to a local folder' : 'download ' + en.name + ' to a local file';
      dl.addEventListener('click', () => {
        // A folder is mirrored recursively; a file goes through the save dialog.
        const job = en.isDir ? App.DownloadDir(sid, en.path) : App.Download(sid, en.path);
        job.then(renderQueue).catch((e) => msg('download: ' + e));
      });
      ac.appendChild(dl);
      const del = document.createElement('button'); del.className = 'danger'; del.textContent = 'del'; del.title = 'delete ' + en.name;
      del.addEventListener('click', () => { if (confirm('delete ' + en.name + '?')) App.Delete(sid, en.path).then(() => loadFiles(cwd())).catch((e) => msg('delete: ' + e)); });
      const ren = document.createElement('button'); ren.textContent = 'rename'; ren.style.marginLeft = '6px';
      ren.addEventListener('click', () => {
        const n = prompt('new name', en.name);
        if (!n || !n.trim()) return;
        App.Rename(sid, en.path, n.trim()).then(() => loadFiles(cwd())).catch((e) => msg('rename: ' + e));
      });
      ac.appendChild(del); ac.appendChild(ren);
      tr.appendChild(nm); tr.appendChild(sz);
      tr.appendChild(cell('dt', en.mtime || '-'));
      tr.appendChild(cell('ow', en.owner || '-'));
      tr.appendChild(cell('gr', en.group || '-'));
      tr.appendChild(cell('pm', en.mode || '-'));
      tr.appendChild(ac);
      frag.appendChild(tr);
    });
    // One swap for the whole directory: /usr and /etc carry hundreds of rows,
    // and appending them to the live table costs a layout pass per row.
    tb.textContent = ''; tb.appendChild(frag);
  } catch (e) { msg('list: ' + e); }
}
function goUp() { loadFiles(parentPath(cwd())); }
function mkdir() {
  const sid = state.currentSid;
  if (!sid) { msg('connect first'); return; }
  const n = prompt('directory name');
  if (!n || !n.trim()) return;
  App.Mkdir(sid, cwd(), n.trim()).then(() => loadFiles(cwd())).catch((e) => msg('mkdir: ' + e));
}
function newFile() {
  if (!state.currentSid) { msg('connect first'); return; }
  const n = prompt('new file name');
  if (!n || !n.trim()) return;
  App.NewFile(state.currentSid, cwd(), n.trim()).then(() => loadFiles(cwd())).catch((e) => msg('new file: ' + e));
}
function upload() { if (!state.currentSid) { msg('connect first'); return; } App.Upload(state.currentSid, cwd()).then(renderQueue).catch((e) => msg('upload: ' + e)); }

// ---------- transfer queue (server-side, polled; docked under the browser) ----------
let queueTimer = null;
function setDockVisible(on) {
  const dock = $('#queueDock'); if (dock) dock.classList.toggle('show', !!on);
}
async function renderQueue() {
  const ul = $('#queueList'); if (!ul) return;
  // Item ids are per session, so a late render for another tab must not leave
  // its rows on screen: their cancel buttons would aim at a different queue.
  const sid = state.currentSid;
  let items = [];
  try { items = sid ? await App.QueueList(sid) : []; } catch (e) { items = []; }
  if (sid !== state.currentSid) return;
  ul.textContent = '';
  if (!items || !items.length) { setDockVisible(false); return; }
  setDockVisible(true);
  items.slice().reverse().forEach((it) => {
    const li = document.createElement('li');
    const top = document.createElement('div'); top.className = 'qtop';
    const kind = document.createElement('span'); kind.className = 'qkind'; kind.textContent = it.direction;
    const name = document.createElement('span'); name.className = 'qname'; name.textContent = it.name;
    const status = document.createElement('span'); status.className = 'qstatus';
    if (it.status === 'Completed') status.textContent = 'done · ' + it.total;
    else if (it.status === 'Transferring') status.textContent = it.progress + '% · ' + it.speed + ' · ETA ' + it.eta;
    else if (it.status === 'Failed') status.textContent = 'error: ' + (it.error || '');
    else status.textContent = it.status + ' · ' + it.transferred + '/' + it.total;
    top.appendChild(kind); top.appendChild(name); top.appendChild(status);
    // A paused row is still cancellable, and pause/resume share one slot: the row
    // is either moving, or waiting for the user to let it move again. A resume
    // continues from the bytes already transferred, not from the start.
    if (it.status === 'Transferring' || it.status === 'Pending' || it.status === 'Paused') {
      const cx = document.createElement('button'); cx.className = 'qcancel'; cx.textContent = 'cancel'; cx.title = 'discard this transfer';
      cx.addEventListener('click', () => App.QueueCancel(sid, it.id).then(renderQueue).catch((e) => msg('cancel: ' + e)));
      top.appendChild(cx);
    }
    const toggle = document.createElement('button'); toggle.className = 'qpause';
    if (it.status === 'Paused') {
      toggle.textContent = 'resume'; toggle.title = 'continue from where it stopped';
      toggle.addEventListener('click', () => App.QueueResume(sid, it.id).then(renderQueue).catch((e) => msg('resume: ' + e)));
    } else if (it.status === 'Transferring' || it.status === 'Pending') {
      toggle.textContent = 'pause'; toggle.title = 'stop here and keep what has already moved';
      toggle.addEventListener('click', () => App.QueuePause(sid, it.id).then(renderQueue).catch((e) => msg('pause: ' + e)));
    }
    if (toggle.textContent) top.appendChild(toggle);
    const bar = document.createElement('div'); bar.className = 'qbar';
    const fill = document.createElement('div'); fill.style.width = (it.status === 'Completed' ? 100 : it.progress) + '%';
    // var() keeps the error bar on the theme palette instead of a fixed red.
    if (it.status === 'Failed' || it.status === 'Cancelled') fill.style.background = 'var(--danger)';
    bar.appendChild(fill);
    li.appendChild(top); li.appendChild(bar);
    ul.appendChild(li);
  });
}
function startQueuePoll() { if (queueTimer) return; renderQueue(); queueTimer = setInterval(renderQueue, 600); }
function stopQueuePoll() { if (queueTimer) { clearInterval(queueTimer); queueTimer = null; } }

// ---------- tunnels ----------
async function refreshTunnels() {
  try { state.tunnels = await App.ListTunnels(); }
  catch (e) { msg('tunnels: ' + e); return; }
  const tb = $('#tunnelBody'); if (!tb) return; tb.textContent = '';
  if (!state.tunnels.length) {
    const tr = document.createElement('tr');
    const td = document.createElement('td'); td.colSpan = 7; td.className = 'empty'; td.textContent = 'No active tunnels';
    tr.appendChild(td); tb.appendChild(tr);
    return;
  }
  const cell = (cls, text) => { const td = document.createElement('td'); if (cls) td.className = cls; td.textContent = text; return td; };
  const editInput = (type, value, placeholder, cls) => {
    const el = document.createElement('input');
    el.type = type; el.value = value; if (placeholder) el.placeholder = placeholder;
    el.className = 'cellEdit' + (cls ? ' ' + cls : '');
    return el;
  };
  state.tunnels.forEach((t) => {
    const tr = document.createElement('tr');
    // A tunnel can outlive nothing but its own session; show the id rather than
    // a blank cell if that session is already gone.
    const sess = state.sessions.find((s) => s.id === t.sessionId);
    // In-row editing: the cells of the row being edited turn into inputs, so
    // the row keeps its place instead of a second form below the table
    // pushing everything down.
    if (state.editingTunnel === t.id) {
      const dynamic = t.kind === 'dynamic';
      const elName = editInput('text', t.name, 'name (optional)');
      const elLocal = editInput('number', t.localPort, 'local port');
      // Bind selector sits beside the port: loopback by default, 0.0.0.0 to
      // let other LAN devices (a phone behind this proxy) use the tunnel.
      const elBind = document.createElement('select'); elBind.className = 'cellEdit bindEdit';
      [['local', 'This device'], ['lan', 'LAN (0.0.0.0)']].forEach(([v, label]) => {
        const o = document.createElement('option'); o.value = v; o.textContent = label; elBind.appendChild(o);
      });
      elBind.value = t.bind === 'lan' ? 'lan' : 'local';
      const elHost = dynamic ? null : editInput('text', t.remoteHost, 'remote host');
      const elRPort = dynamic ? null : editInput('number', t.remotePort, 'port', 'port');
      const tdName = document.createElement('td'); tdName.appendChild(elName);
      const tdLocal = document.createElement('td'); tdLocal.className = 'portEdit';
      tdLocal.appendChild(elLocal); tdLocal.appendChild(elBind);
      tr.appendChild(tdName);
      tr.appendChild(cell('muted', t.type));
      tr.appendChild(cell(t.running ? 'run' : 'stopped', t.running ? 'running' : 'stopped'));
      tr.appendChild(tdLocal);
      if (dynamic) {
        // A SOCKS forward dials no fixed destination, so there is nothing to
        // edit in the cell where a local one shows host + port inputs.
        tr.appendChild(cell('muted', 'any (via SOCKS)'));
      } else {
        const tdDest = document.createElement('td'); tdDest.className = 'destEdit';
        tdDest.appendChild(elHost); tdDest.appendChild(elRPort);
        tr.appendChild(tdDest);
      }
      tr.appendChild(cell('', sess ? sess.name : t.sessionId));
      const ac = document.createElement('td'); ac.className = 'tacts';
      const save = document.createElement('button'); save.textContent = 'Save'; save.className = 'primary'; save.title = 'apply and restart this forward';
      const cancel = document.createElement('button'); cancel.textContent = 'Cancel'; cancel.title = 'discard the changes';
      const commit = () => {
        // Guard against a second Enter (key repeat, or Save clicked twice)
        // while the update is still in flight: every call stops and re-binds
        // the forward, so repeats would drop live tunneled connections.
        if (state.editingTunnel !== t.id) return;
        const name = elName.value.trim();
        const lp = parsePort(elLocal.value);
        let rh = '', rp = 0;
        if (!dynamic) { rh = elHost.value.trim(); rp = parsePort(elRPort.value); }
        if (lp === null) { msg('local port must be 1-65535'); return; }
        if (!dynamic && !rh) { msg('remote host required'); return; }
        if (!dynamic && rp === null) { msg('remote port must be 1-65535'); return; }
        state.editingTunnel = '';
        App.UpdateTunnel(t.id, name, lp, rh, rp, elBind.value).then(() => {
          msg('tunnel updated');
          refreshTunnels();
        }).catch((e) => {
          // Refresh even on failure: a rejected update leaves the definition
          // stopped with the new values, and the row should say so.
          msg('update: ' + e);
          refreshTunnels();
        });
      };
      const cancelEdit = () => { state.editingTunnel = ''; refreshTunnels(); };
      save.addEventListener('click', commit);
      cancel.addEventListener('click', cancelEdit);
      [elName, elLocal, elHost, elRPort].forEach((el) => { if (!el) return; el.addEventListener('keydown', (ev) => {
        if (ev.repeat) return;
        if (ev.key === 'Enter') commit();
        else if (ev.key === 'Escape') cancelEdit();
      }); });
      ac.appendChild(save);
      cancel.style.marginLeft = '6px'; ac.appendChild(cancel);
      tr.appendChild(ac);
      tb.appendChild(tr);
      requestAnimationFrame(() => elName.focus());
      return;
    }
    tr.appendChild(cell('nm', t.name));
    tr.appendChild(cell('muted', t.type));
    tr.appendChild(cell(t.running ? 'run' : 'stopped', t.running ? 'running' : 'stopped'));
    tr.appendChild(cell(t.bind === 'lan' ? 'lanb' : '', t.localPort + (t.bind === 'lan' ? ' \u00b7 LAN' : '')));
    tr.appendChild(cell('', t.remote));
    tr.appendChild(cell('', sess ? sess.name : t.sessionId));
    const ac = document.createElement('td'); ac.className = 'tacts';
    // Stop only releases the listener; the definition stays so the row survives
    // and can be started again. "remove" is what deletes it.
    const toggle = document.createElement('button');
    toggle.textContent = t.running ? 'stop' : 'start';
    if (t.running) toggle.className = 'danger';
    toggle.title = t.running ? 'stop this forward, keep the definition' : 'start this forward again';
    toggle.addEventListener('click', () => {
      const job = t.running ? App.StopTunnel(t.id) : App.StartTunnel(t.id);
      job.then(refreshTunnels).catch((e) => { msg('tunnel: ' + e); refreshTunnels(); });
    });
    const edit = document.createElement('button'); edit.textContent = 'edit'; edit.title = 'change this forward in place';
    edit.addEventListener('click', () => { state.editingTunnel = t.id; refreshTunnels(); });
    const rm = document.createElement('button'); rm.textContent = 'remove'; rm.title = 'delete this tunnel definition';
    rm.addEventListener('click', () => {
      // Tunnels are worth a confirm: a running forward may carry live traffic
      // and its settings would have to be retyped from memory after a mis-click.
      if (!confirm('remove ' + t.name + '?')) return;
      if (state.editingTunnel === t.id) state.editingTunnel = '';
      App.CloseTunnel(t.id).then(() => { msg('tunnel removed'); refreshTunnels(); }).catch((e) => msg('remove: ' + e));
    });
    [toggle, edit, rm].forEach((b, i) => { if (i) b.style.marginLeft = '6px'; ac.appendChild(b); });
    tr.appendChild(ac);
    tb.appendChild(tr);
  });
}

function showTunnelForm(on) {
  const f = $('#tunnelForm'); if (!f) return;
  f.hidden = !on;
  if (on) { const el = $('#tLocal'); if (el) el.focus(); }
}

// clearTunnelForm empties the new-tunnel fields and hides the form.
function clearTunnelForm() {
  ['#tName', '#tLocal', '#tHost', '#tPort'].forEach((s) => { const el = $(s); if (el) el.value = ''; });
  showTunnelForm(false);
}

// openTunnelForm shows the blank form used to create a new forward. Editing an
// existing tunnel happens in its own row instead (see refreshTunnels).
function openTunnelForm() {
  if (!state.currentSid) { msg('connect first'); return; }
  const ty = $('#tType'); if (ty) ty.value = 'local';
  const bd = $('#tBind'); if (bd) bd.value = 'local';
  syncTunnelFormFields();
  showTunnelForm(true);
}

// syncTunnelFormFields hides the destination inputs for a SOCKS forward - it
// has none until a client asks for one through the proxy.
function syncTunnelFormFields() {
  const dyn = $('#tType') && $('#tType').value === 'dynamic';
  const h = $('#tHost'), p = $('#tPort');
  if (h) h.hidden = dyn;
  if (p) p.hidden = dyn;
}

function submitTunnel() {
  if (!state.currentSid) { msg('connect first'); return; }
  const name = $('#tName').value.trim();
  const lp = parsePort($('#tLocal').value);
  if (lp === null) { msg('local port must be 1-65535'); return; }
  const bd = $('#tBind');
  const bind = bd && bd.value === 'lan' ? 'lan' : 'local';
  let job;
  if ($('#tType') && $('#tType').value === 'dynamic') {
    job = App.OpenDynamicTunnel(state.currentSid, name, lp, bind);
  } else {
    const rh = $('#tHost').value.trim(), rp = parsePort($('#tPort').value);
    if (!rh) { msg('remote host required'); return; }
    if (rp === null) { msg('remote port must be 1-65535'); return; }
    job = App.OpenTunnel(state.currentSid, name, lp, rh, rp, bind);
  }
  job.then(() => {
    msg('tunnel open');
    clearTunnelForm();
    refreshTunnels();
  }).catch((e) => {
    msg('tunnel: ' + e);
    refreshTunnels();
  });
}

function stopAllTunnels() {
  if (!state.currentSid) { msg('connect first'); return; }
  App.StopAllTunnels(state.currentSid).then((n) => { msg(n + ' tunnel(s) stopped'); refreshTunnels(); }).catch((e) => msg('stop all: ' + e));
}

// ---------- settings ----------
// Only the terminal font lives here; the theme button and the file-pane
// toggles already sit with the surfaces they control.
function refreshSettings() {
  const r = $('#setTermFont');
  if (r) r.value = state.term ? state.term.options.fontSize : termFontSize();
  const o = $('#setTermFontVal');
  if (o && r) o.textContent = r.value + ' px';
}

function wireSettings() {
  const r = $('#setTermFont');
  if (r) r.addEventListener('input', () => {
    const o = $('#setTermFontVal'); if (o) o.textContent = r.value + ' px';
    setTermFont(parseInt(r.value, 10));
  });
  const ra = $('#setResetAll');
  if (ra) ra.addEventListener('click', resetPreferences);
}

// Keys written by the settings surfaces (font, theme, follow, hidden, pane).
// Saved sessions, tunnels and trusted hosts are data, not preferences - they
// are deliberately untouched by a reset.
const PREF_KEYS = ['gravel.termFontSize', 'gravel.theme', 'gravel.followTerminalFolder', 'gravel.showHidden', 'gravel.paneWidth'];

// resetPreferences clears the stored prefs and re-applies every default live:
// font 14px, dark theme, follow-on, hidden-off, pane at 720px.
function resetPreferences() {
  PREF_KEYS.forEach((k) => { try { localStorage.removeItem(k); } catch (e) {} });
  setTermFont(FONT_DEFAULT);
  applyTheme('dark');
  const ft = $('#followTerm'); if (ft) { ft.checked = true; ft.dispatchEvent(new Event('change')); }
  const sh = $('#showHidden'); if (sh) { sh.checked = false; sh.dispatchEvent(new Event('change')); }
  setPaneWidth(PANE_DEFAULT);
  refitTerminal();
  refreshSettings();
  msg('settings reset to defaults');
}

// ---------- logs (live view of everything the backend a.log() writes) ----------
// The backend keeps a 500-line ring and mirrors each new line here as a
// "log:line" event, so the tab follows along even while it is not open.
let logLines = [];

EventsOn('log:line', (line) => {
  logLines.push(line);
  if (logLines.length > 500) logLines.splice(0, logLines.length - 500);
  if (state.view === 'logs') renderLogs();
});

async function refreshLogs() {
  try { logLines = await App.ListLogs() || []; } catch (e) {}
  renderLogs();
}

function logFollowOn() { const el = $('#logFollow'); return !!(el && el.checked); }

function renderLogs() {
  const box = $('#logBox'); if (!box) return;
  box.textContent = logLines.join('\n');
  if (logFollowOn()) box.scrollTop = box.scrollHeight;
}

function clearLogView() {
  logLines = [];
  renderLogs();
  msg('log view cleared');
}

// dumpLogs writes only the current session's lines (what the Logs tab shows,
// from app start to now) to a file the user picks. The per-launch file in
// AppData keeps recording regardless.
function dumpLogs() {
  App.DumpLogs().then((p) => { if (p) msg('log dumped to ' + p); }).catch((e) => msg('dump: ' + e));
}

// ---------- resizable file pane ----------
const PANE_KEY = 'gravel.paneWidth';
// Bounds match the min-width/max-width guards on #filePane in style.css.
// The seven columns need 140+54+112+72+72+84+164 = 698px; 720 leaves room for the
// pane border and sub-pixel rounding without a sideways scrollbar.
const PANE_MIN = 320, PANE_MAX = 900, PANE_DEFAULT = 720;

function setPaneWidth(px) {
  const w = Math.min(PANE_MAX, Math.max(PANE_MIN, Math.round(px)));
  document.documentElement.style.setProperty('--pane-w', w + 'px');
  try { localStorage.setItem(PANE_KEY, String(w)); } catch (e) {}
  return w;
}

// Every fit pushes the resulting size to the active session unconditionally.
// xterm's own onResize only fires when the widget's own cols change, so a
// stale fit (hidden container, late layout) could leave the remote wrapping at
// a width the screen no longer shows - the clipped lines and blank gaps this
// prevents.
function refitTerminal() {
  if (!state.fit) return;
  try { state.fit.fit(); } catch (e) {}
  if (state.currentSid && state.term) App.ResizeSession(state.currentSid, state.term.cols, state.term.rows);
}

function initSplitter() {
  const sp = $('#splitter'); if (!sp) return;
  const saved = parseInt(localStorage.getItem(PANE_KEY) || '', 10);
  if (saved >= PANE_MIN && saved <= PANE_MAX) setPaneWidth(saved);
  let dragging = false;
  sp.addEventListener('pointerdown', (e) => {
    dragging = true;
    sp.classList.add('dragging');
    document.body.classList.add('resizing');
    try { sp.setPointerCapture(e.pointerId); } catch (err) {}
    e.preventDefault();
  });
  sp.addEventListener('pointermove', (e) => {
    if (!dragging) return;
    setPaneWidth(e.clientX - sp.parentElement.getBoundingClientRect().left);
  });
  const stop = (e) => {
    if (!dragging) return;
    dragging = false;
    sp.classList.remove('dragging');
    document.body.classList.remove('resizing');
    try { sp.releasePointerCapture(e.pointerId); } catch (err) {}
    refitTerminal(); // reflow the terminal once, not on every move
  };
  sp.addEventListener('pointerup', stop);
  sp.addEventListener('pointercancel', stop);
  sp.addEventListener('dblclick', () => { setPaneWidth(PANE_DEFAULT); refitTerminal(); });
}

// ---------- wire events ----------
// initFollowToggle restores the persisted preference; when switching it back on
// the pane is snapped to wherever the terminal session currently is.
function initFollowToggle() {
  const el = $('#followTerm'); if (!el) return;
  const saved = localStorage.getItem(FOLLOW_KEY);
  if (saved !== null) el.checked = saved === '1';
  el.addEventListener('change', () => {
    localStorage.setItem(FOLLOW_KEY, el.checked ? '1' : '0');
    if (el.checked && state.currentSid) loadFiles(cwd());
  });
}

// applyTheme drives both the CSS palette (data-theme on <html>, read by the
// inline script in index.html before first paint so there is no flash) and the
// xterm theme, which is derived from the same custom properties.
function applyTheme(name) {
  document.documentElement.dataset.theme = name;
  try { localStorage.setItem(THEME_KEY, name); } catch (e) {}
  if (state.term) { try { state.term.options.theme = termTheme(); } catch (e) {} }
  // The sliding thumb and the highlighted label are pure CSS on data-theme, so
  // the only thing left is telling assistive tech which segment is pressed.
  document.querySelectorAll('#themeToggle .tseg').forEach((b) => {
    b.setAttribute('aria-pressed', b.dataset.theme === name ? 'true' : 'false');
  });
}

function initTheme() {
  applyTheme(document.documentElement.dataset.theme === 'light' ? 'light' : 'dark');
  document.querySelectorAll('#themeToggle .tseg').forEach((b) => {
    b.addEventListener('click', () => applyTheme(b.dataset.theme));
  });
}

// initShowHidden restores the dotfile preference; changing it only re-renders the
// listing that is already loaded, so no extra round trip is needed on first open.
function initShowHidden() {
  const el = $('#showHidden'); if (!el) return;
  el.checked = localStorage.getItem(HIDDEN_KEY) === '1';
  el.addEventListener('change', () => {
    try { localStorage.setItem(HIDDEN_KEY, el.checked ? '1' : '0'); } catch (e) {}
    if (state.currentSid) loadFiles(cwd());
  });
}

document.querySelectorAll('.navitem').forEach((b) => b.addEventListener('click', () => switchView(b.dataset.view)));
$('#btnConnectServer').addEventListener('click', doConnect);
$('#btnLoadServer').addEventListener('click', () => {
  if (!state.selectedProfile) { msg('select a saved session first'); return; }
  fillForm(state.selectedProfile);
});
$('#btnSaveServer').addEventListener('click', saveServer);
$('#btnDeleteServer').addEventListener('click', deleteServer);
$('#btnImportHosts').addEventListener('click', openImportDialog);
$('#importGo').addEventListener('click', runImport);
$('#importPickFile').addEventListener('click', pickImportFile);
$('#importPutty').addEventListener('click', importFromPutty);
$('#importCancel').addEventListener('click', () => $('#importDlg').close());
$('#svAuth').addEventListener('change', (e) => { $('#svKeyWrap').hidden = e.target.value !== 'key'; });
$('#btnPickKey').addEventListener('click', async () => {
  try { const p = await App.PickSSHKey(); if (p) $('#svKey').value = p; }
  catch (e) { msg('key picker: ' + e); }
});
$('#btnUp').addEventListener('click', goUp);
$('#btnMkdir').addEventListener('click', mkdir);
$('#btnNewFile').addEventListener('click', newFile);
$('#btnUpload').addEventListener('click', upload);
$('#btnRefresh').addEventListener('click', () => loadFiles(cwd()));
$('#btnAddTunnel').addEventListener('click', submitTunnel);
$('#btnNewTunnel').addEventListener('click', openTunnelForm);
$('#tType').addEventListener('change', syncTunnelFormFields);
wireSettings();
$('#btnCancelTunnel').addEventListener('click', clearTunnelForm);
$('#btnStopAllTunnels').addEventListener('click', stopAllTunnels);
$('#btnLogRefresh').addEventListener('click', refreshLogs);
$('#btnLogClear').addEventListener('click', clearLogView);
$('#btnLogDump').addEventListener('click', dumpLogs);
$('#btnStartAllTunnels').addEventListener('click', () => {
  if (!state.currentSid) { msg('connect first'); return; }
  App.StartAllTunnels(state.currentSid).then((n) => { msg(n + ' tunnel(s) started'); refreshTunnels(); }).catch((e) => msg('start all: ' + e));
});
$('#btnQueueClear').addEventListener('click', () => App.QueueClear(state.currentSid).then(renderQueue).catch((e) => msg('clear queue: ' + e)));
$('#tofuAccept').addEventListener('click', acceptTOFU);
$('#tofuReject').addEventListener('click', () => { $('#tofu').close(); state.retryConnect = null; msg('connection rejected'); });

// Links open in the user's real browser. Letting the webview navigate would
// replace the running app with a web page and lose the terminal session.
document.querySelectorAll('a[data-url]').forEach((a) => a.addEventListener('click', (e) => {
  e.preventDefault();
  try { BrowserOpenURL(a.dataset.url); } catch (err) { msg('could not open ' + a.dataset.url); }
}));

// No zoom gestures at all: Ctrl + wheel and Ctrl + +/- were removed because a
// resize that the remote has not acknowledged garbles its line wrapping; the
// window itself can still be resized, which reflows correctly via fit+resize.

// ---------- boot ----------
// The session list is polled rather than pushed. The handle is kept so a second
// boot() in the same document can never stack a second poller on top of the
// first, the way the transfer queue poll already refuses to double-start.
let sessionPoll = 0;

async function boot() {
  initTheme();
  initSplitter();
  initFollowToggle();
  initShowHidden();
  try { const v = await App.VersionInfo(); $('#aboutVer').textContent = v; } catch (e) { msg('version: ' + e); }
  await refreshProfiles();
  await refreshSessions();
  if (sessionPoll) clearInterval(sessionPoll);
  sessionPoll = setInterval(() => { refreshSessions().catch(() => {}); }, 5000);
}
boot();
