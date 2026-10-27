package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"gravel/internal/app"
	"gravel/internal/follow"
	"gravel/internal/logging"
	"gravel/internal/sessionstore"
	"gravel/internal/sftpclient"
	"gravel/internal/socks"
	"gravel/internal/sshclient"
	"gravel/internal/transfer"
)

// Version is the product version string surfaced to the UI.
const Version = "4.6.0"

// maxConcurrentTransfers bounds how many files move at once in the queue.
const maxConcurrentTransfers = 2

// maxDownloadFiles caps a folder download so pointing it at something like /usr
// fails with a message instead of queueing tens of thousands of transfers.
const maxDownloadFiles = 2000

// maxAuthAttempts bounds the PuTTY-style re-prompt loop after a rejected
// password. Servers cap this themselves (OpenSSH MaxAuthTries defaults to 6);
// three keeps a mistyped password recoverable without letting a locked-out
// account cycle forever.
const maxAuthAttempts = 3

// keepLogs bounds how many per-launch log files stay in the config folder, so a
// long-lived install does not accumulate one for every start it ever had. The
// security value runs the other way too: these files hold connection history.
const keepLogs = 20

// App is the Wails-bound root object. Every exported method becomes callable
// from the frontend as window.go.main.App.<Method>.
type App struct {
	ctx      context.Context
	mu       sync.Mutex
	sessions map[string]*appSession
	order    []string
	tunnels  map[string]*tunnel
	kh       *sshclient.KnownHosts
	store    *sessionstore.Store
	logger   *logging.Logger

	// connGen bumps with every Connect/ConnectKey so an earlier attempt that
	// is still dialling (a 5s probe timeout, a slow handshake) can tell it has
	// been superseded and stay quiet instead of splashing its FATAL ERROR onto
	// the newer attempt's login prompt.
	connGen uint64

	pendingAuth  chan string
	pendingCreds map[string]cachedCred
}

// newConnAttempt registers a connection attempt and returns its ticket.
func (a *App) newConnAttempt() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connGen++
	return a.connGen
}

// superseded reports whether a newer Connect started after this ticket.
func (a *App) superseded(gen uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connGen != gen
}

type appSession struct {
	ID        string
	Ctrl      *app.Controller
	Profile   sessionstore.Profile
	FilesPath string
	cancel    func()
	queue     *transfer.TransferQueue
}

type cachedCred struct {
	user string
	pass string
	at   time.Time
}

type tunnel struct {
	ID         string
	SessionID  string
	Name       string
	LocalPort  int
	RemoteHost string
	RemotePort int
	// dynamic marks a SOCKS5 (-D) forward: the remote host/port are unused
	// and each client request carries its own destination.
	// bindAll listens on every interface (0.0.0.0) instead of loopback, so
	// other devices on the LAN can use this tunnel too.
	dynamic  bool
	bindAll  bool
	listener net.Listener
}

// running reports whether the forward is live. A nil listener means the
// definition is kept but stopped, so the row survives a Stop and can start
// again without retyping anything.
func (t *tunnel) running() bool { return t.listener != nil }

// takeListener detaches a tunnel's listener under a.mu and hands it back for
// closing outside the lock. The handover has to be inside the lock: reading
// t.listener after releasing it races StartTunnel installing a new one, and the
// row could then be deleted while its freshly bound port belongs to nobody - a
// listener no code path will ever close again.
func (a *App) takeListener(t *tunnel) net.Listener {
	a.mu.Lock()
	defer a.mu.Unlock()
	ln := t.listener
	t.listener = nil
	return ln
}

// closeListenerOf releases a tunnel's bound port. It is safe on a stopped
// forward, whose listener is nil - without the guard, closing a session that
// held a stopped tunnel would panic on the nil interface.
func (a *App) closeListenerOf(t *tunnel) error {
	if ln := a.takeListener(t); ln != nil {
		return ln.Close()
	}
	return nil
}

// localAddr and remoteAddr are the addresses actually bound locally and dialled
// through the SSH connection. The parts are stored rather than the strings so
// the UI can show and re-edit them without parsing. A dynamic forward dials no
// fixed target, so it has no remote address to name. A LAN-bound tunnel
// listens on every interface; the default stays loopback-only.
func (t *tunnel) localAddr() string {
	host := "127.0.0.1"
	if t.bindAll {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, t.LocalPort)
}
func (t *tunnel) remoteAddr() string {
	if t.dynamic {
		return ""
	}
	return fmt.Sprintf("%s:%d", t.RemoteHost, t.RemotePort)
}

// kind is the machine-readable tunnel type the frontend branches on.
func (t *tunnel) kind() string {
	if t.dynamic {
		return "dynamic"
	}
	return "local"
}

// tunnelTypeLocal names a fixed local forward, shown in the Type column so the
// table reads the same way as the others do.
const tunnelTypeLocal = "Local (-L)"

// tunnelTypeDynamic names a SOCKS5 forward: any destination, chosen per request
// by whatever client connects to the local port.
const tunnelTypeDynamic = "Dynamic (SOCKS)"

// typeName is the display label for the Type column.
func (t *tunnel) typeName() string {
	if t.dynamic {
		return tunnelTypeDynamic
	}
	return tunnelTypeLocal
}

// validPort guards a port the user typed. Left unchecked, a zero local port
// makes net.Listen pick an ephemeral port and the row then advertises a port it
// does not actually hold; a zero remote port only fails later, on every
// connection attempt, with a message that does not point back at the form.
func validPort(kind string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s port %d out of range (1-65535)", kind, port)
	}
	return nil
}

// NewApp creates the App instance.
func NewApp() *App {
	return &App{
		sessions:     map[string]*appSession{},
		order:        []string{},
		tunnels:      map[string]*tunnel{},
		pendingCreds: map[string]cachedCred{},
		logger:       logging.NewLogger(),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	dir, err := sessionstore.DefaultDir()
	if err != nil {
		// Without a config folder there is no store, no known hosts and no log
		// file. Say so in the in-memory ring so the Logs tab shows the reason
		// every connection fails instead of the app looking quietly broken.
		a.log("startup failed: config directory unavailable: %v", err)
		return
	}
	kh := sshclient.NewKnownHosts(dir)
	kh.Interactive = true
	_ = kh.Load()
	a.kh = kh
	st := sessionstore.NewStore(dir)
	_ = st.Load()
	a.store = st
	// One log file per app launch (gravel-2006-01-02-150405.log) instead of
	// one ever-growing gravel.log; the timestamped name is the file's own
	// session header on disk.
	logName := "gravel-" + time.Now().Format("2006-01-02-150405") + ".log"
	_ = a.logger.Start(filepath.Join(dir, logName))
	// One file per launch adds up without bound, so trim the oldest ones. The
	// file just opened is the newest of them all, so this launch's log is never a
	// candidate, and only gravel-*.log / bedrock-*.log files are considered.
	if removed, err := sessionstore.PruneLogs(dir, keepLogs); err != nil {
		a.log("log cleanup skipped: %v", err)
	} else if removed > 0 {
		a.log("pruned %d older log file(s), keeping the newest %d", removed, keepLogs)
	}
	// Mirror every log line to the UI so the Logs tab follows along live.
	a.logger.OnLine(func(line string) { runtime.EventsEmit(a.ctx, "log:line", line) })
	a.log("startup; config dir=%s", dir)
}

func (a *App) shutdown(_ context.Context) {
	a.log("shutdown")
	a.mu.Lock()
	list := make([]*appSession, 0, len(a.sessions))
	for _, s := range a.sessions {
		list = append(list, s)
	}
	tunnels := make([]*tunnel, 0, len(a.tunnels))
	for _, t := range a.tunnels {
		tunnels = append(tunnels, t)
	}
	a.sessions = map[string]*appSession{}
	a.order = nil
	a.tunnels = map[string]*tunnel{}
	a.mu.Unlock()
	for _, t := range tunnels {
		_ = a.closeListenerOf(t)
	}
	for _, s := range list {
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.Ctrl.Close()
	}
	a.logger.Stop()
}

func (a *App) log(format string, args ...interface{}) {
	if a.logger != nil {
		a.logger.Write(fmt.Sprintf(format, args...))
	}
}

// VersionInfo returns the bare product version for the About screen's
// "Version:" row; the h1 heading keeps showing just the name.
func (a *App) VersionInfo() string { return Version }

// ListLogs returns the buffered application log lines (oldest first) for the
// Logs tab. They are the same entries written to this launch's gravel-*.log.
func (a *App) ListLogs() []string {
	if a.logger == nil {
		return nil
	}
	return a.logger.Lines()
}

// LogUI records a frontend feedback line ("saved", "tunnel updated", errors)
// in the application log, so the Logs tab has a single source for every line
// it shows and Dump exports the same set the user saw on screen.
func (a *App) LogUI(text string) {
	a.log("%s", text)
}

// DumpLogs saves the buffered log lines - everything this session has sent to
// the Logs tab, up to the logger's 500-line ring - to a file the user picks.
// The per-launch file in AppData keeps recording regardless; this is a copy.
// Returns "" when the dialog is cancelled.
func (a *App) DumpLogs() (string, error) {
	if a.logger == nil {
		return "", fmt.Errorf("logger unavailable")
	}
	dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Dump current session logs",
		DefaultFilename: time.Now().Format("gravel-dump-2006-01-02-1504") + ".log",
		Filters: []runtime.FileFilter{
			{DisplayName: "Log files (*.log)", Pattern: "*.log"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil || dest == "" {
		return "", err
	}
	lines := a.logger.Lines()
	data := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		a.log("log dump to %s failed: %v", dest, err)
		return "", err
	}
	a.log("logs dumped to %s (%d lines)", dest, len(lines))
	return dest, nil
}

// ---------- interactive auth ----------

func (a *App) promptAuth(question string, echo bool) (string, error) {
	ch := make(chan string, 1)
	a.mu.Lock()
	if a.pendingAuth != nil {
		a.mu.Unlock()
		return "", fmt.Errorf("another auth prompt is active")
	}
	a.pendingAuth = ch
	a.mu.Unlock()

	runtime.EventsEmit(a.ctx, "auth:prompt", map[string]any{"question": question, "echo": echo})

	select {
	case ans := <-ch:
		a.clearAuthSlot(ch)
		return ans, nil
	case <-time.After(120 * time.Second):
		a.clearAuthSlot(ch)
		return "", fmt.Errorf("auth prompt timed out")
	}
}

// clearAuthSlot releases the prompt slot only if it still belongs to ch. A
// cancelled or timed-out prompt must not wipe the channel a newer attempt has
// already installed, or that attempt's question could never be answered.
func (a *App) clearAuthSlot(ch chan string) {
	a.mu.Lock()
	if a.pendingAuth == ch {
		a.pendingAuth = nil
	}
	a.mu.Unlock()
}

// noticeAuth writes an authentication status line straight into the terminal,
// outside the question/answer cycle. A rejected password has to be visible
// where the user is looking, not only in the log.
func (a *App) noticeAuth(text string) {
	a.log("%s", text)
	runtime.EventsEmit(a.ctx, "auth:notice", text)
}

// AuthAnswer delivers the user's response to the currently active auth prompt.
func (a *App) AuthAnswer(answer string) {
	a.mu.Lock()
	ch := a.pendingAuth
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- answer:
		default:
		}
	}
}

// credential prompts for a login name and password in the terminal, in PuTTY's
// wording. A short-lived cache keyed by addr lets a TOFU retry reuse the
// just-entered credentials without re-prompting.
func (a *App) credential(host, addr string) (user, pass string) {
	a.mu.Lock()
	c, ok := a.pendingCreds[addr]
	a.mu.Unlock()
	if ok && time.Since(c.at) < 2*time.Minute {
		return c.user, c.pass
	}
	u, err := a.promptAuth("login as:", true)
	if err != nil || u == "" {
		return "", ""
	}
	p, err := a.promptAuth(fmt.Sprintf("%s@%s's password:", u, host), false)
	if err != nil {
		return "", ""
	}
	a.mu.Lock()
	a.pendingCreds[addr] = cachedCred{user: u, pass: p, at: time.Now()}
	a.mu.Unlock()
	return u, p
}

func (a *App) clearCred(addr string) {
	a.mu.Lock()
	delete(a.pendingCreds, addr)
	a.mu.Unlock()
}

// ---------- connect ----------

// probeConnectTimeout bounds the reachability check that runs before any
// credential prompt. A refused port answers instantly; this only waits out
// firewalled addresses that swallow the SYN.
const probeConnectTimeout = 5 * time.Second

// probeReachable opens and closes a bare TCP connection to addr. PuTTY dials
// the server before it ever prints "login as:", so a machine that is down or
// has SSH switched off fails at once instead of asking for a username and
// password nobody can use. The real dial follows moments later; a server that
// dies in between is still reported by dialAndOpen.
func (a *App) probeReachable(addr string) error {
	d, err := net.DialTimeout("tcp", addr, probeConnectTimeout)
	if err != nil {
		// PuTTY words this failure "FATAL ERROR: Unable to establish connection
		// to host"; keep the same wording so it is recognisable in the terminal.
		return fmt.Errorf("FATAL ERROR: Unable to establish connection to %s", addr)
	}
	_ = d.Close()
	return nil
}

// ConnectResult is the return value of Connect/ConnectKey.
type ConnectResult struct {
	SID         string `json:"sid,omitempty"`
	Error       string `json:"error,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Addr        string `json:"addr,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	KeyType     string `json:"keyType,omitempty"`
}

// cancelStaleAuth aborts a login prompt left waiting from an earlier Connect
// attempt. Without it, a fresh Connect to a dead server printed its FATAL ERROR
// straight onto the old "login as:" line and the abandoned goroutine kept the
// auth slot for its full 120s timeout. The slot is released here, before the
// caller continues, so the new attempt's promptAuth never sees the stale one.
func (a *App) cancelStaleAuth() {
	a.mu.Lock()
	ch := a.pendingAuth
	a.pendingAuth = nil
	a.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- "":
	default:
		// An answer was already queued; the sender's Connect will observe the
		// superseded ticket before dialling.
	}
	// The blocked promptAuth takes the empty answer as a cancellation and its
	// Connect returns "cancelled"; tell the terminal to drop the prompt line too.
	runtime.EventsEmit(a.ctx, "auth:cancel", "")
}

// Connect performs a PuTTY-style interactive login (login name + password are
// prompted in the terminal; nothing is ever stored). The server is probed
// first, so a host that is down fails immediately without any prompts. A
// rejected password re-prompts for the password, keeping the login name, up to
// maxAuthAttempts.
func (a *App) Connect(host string, port int) ConnectResult {
	if port == 0 {
		port = 22
	}
	// Same rule the tunnel forms already apply: refuse before touching anything,
	// so a port no socket could bind never tears down a live login prompt on its
	// way to a dial that could only fail.
	if err := validPort("server", port); err != nil {
		a.noticeAuth(err.Error())
		return ConnectResult{Error: err.Error(), Kind: "connect"}
	}
	a.cancelStaleAuth()
	gen := a.newConnAttempt()
	addr := fmt.Sprintf("%s:%d", host, port)
	if err := a.probeReachable(addr); err != nil {
		if a.superseded(gen) {
			// A newer Connect is already on screen; log the stale failure but
			// do not write over it.
			a.log("%v (attempt superseded)", err)
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		// Show the failure where the user is looking (the terminal) as well as
		// in the status line, instead of prompting for a login nobody needs.
		a.noticeAuth(err.Error())
		return ConnectResult{Error: err.Error(), Kind: "connect"}
	}
	user, pass := a.credential(host, addr)
	if user == "" {
		a.log("connect cancelled: no credentials for %s", addr)
		return ConnectResult{Error: "cancelled", Kind: "cancelled"}
	}
	if a.superseded(gen) {
		// A newer Connect started while credentials were being typed (or served
		// from the short-lived cache); dialling now would paint its session
		// over the newer attempt's screen.
		a.log("connect %s superseded before dial", addr)
		return ConnectResult{Error: "cancelled", Kind: "cancelled"}
	}
	prof := sessionstore.Profile{ID: user + "@" + host, Name: user + "@" + host, Host: host, Port: port, User: user, Auth: sessionstore.AuthPassword}
	for attempt := 1; ; attempt++ {
		res := a.dialAndOpen(prof, sshclient.Credential{Password: pass})
		if res.Kind == "unknown_host" {
			// Leave the short-lived cache intact: the retry after the user trusts
			// the key must not ask for the login name and password again.
			return res
		}
		a.clearCred(addr)
		if res.Kind != "auth" {
			return res
		}
		if a.superseded(gen) {
			a.log("auth loop for %s abandoned: newer connect attempt", addr)
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		if attempt == maxAuthAttempts {
			a.noticeAuth("Access denied: too many incorrect passwords.")
			return ConnectResult{Error: "too many incorrect passwords", Kind: "auth"}
		}
		// Empty answer or Ctrl-C gives up rather than burning the remaining tries.
		a.noticeAuth("Access denied, please try again.")
		p, err := a.promptAuth(fmt.Sprintf("%s@%s's password:", user, host), false)
		if err != nil || p == "" {
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		pass = p
	}
}

// NewSessionLike opens another session to the same server as an existing one,
// reusing the short-lived credential cache when it is still fresh (so a quick
// duplicate often needs no re-login). It backs the tab's right-click
// "Duplicate terminal".
func (a *App) NewSessionLike(sid string) ConnectResult {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	var prof sessionstore.Profile
	if ok {
		prof = s.Profile
	}
	a.mu.Unlock()
	if !ok {
		return ConnectResult{Error: "no such session", Kind: "internal"}
	}
	if prof.Auth == sessionstore.AuthKey {
		return a.ConnectKey(prof.Host, prof.Port, prof.KeyPath)
	}
	return a.Connect(prof.Host, prof.Port)
}

// ConnectKey authenticates using a private key file. The login name and the key
// passphrase are both prompted in the terminal, like Connect does - again only
// after the server proves reachable. A wrong passphrase is re-prompted up to
// maxAuthAttempts times, matching the behaviour of Connect for passwords.
func (a *App) ConnectKey(host string, port int, keyPath string) ConnectResult {
	if port == 0 {
		port = 22
	}
	if err := validPort("server", port); err != nil {
		a.noticeAuth(err.Error())
		return ConnectResult{Error: err.Error(), Kind: "connect"}
	}
	a.cancelStaleAuth()
	gen := a.newConnAttempt()
	addr := fmt.Sprintf("%s:%d", host, port)
	if err := a.probeReachable(addr); err != nil {
		if a.superseded(gen) {
			a.log("%v (attempt superseded)", err)
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		a.noticeAuth(err.Error())
		return ConnectResult{Error: err.Error(), Kind: "connect"}
	}
	user, err := a.promptAuth("login as:", true)
	if err != nil || user == "" {
		return ConnectResult{Error: "cancelled", Kind: "cancelled"}
	}
	pass, _ := a.promptAuth("key passphrase (leave empty if none):", false)
	if a.superseded(gen) {
		a.log("connect %s superseded before dial", addr)
		return ConnectResult{Error: "cancelled", Kind: "cancelled"}
	}
	prof := sessionstore.Profile{ID: user + "@" + host, Name: user + "@" + host, Host: host, Port: port, User: user, Auth: sessionstore.AuthKey, KeyPath: keyPath}
	for attempt := 1; ; attempt++ {
		res := a.dialAndOpen(prof, sshclient.Credential{Passphrase: pass})
		if res.Kind == "unknown_host" {
			return res
		}
		if res.Kind != "auth" {
			return res
		}
		if a.superseded(gen) {
			a.log("auth loop for %s abandoned: newer connect attempt", addr)
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		if attempt == maxAuthAttempts {
			a.noticeAuth("Access denied: too many incorrect passphrases.")
			return ConnectResult{Error: "too many incorrect passphrases", Kind: "auth"}
		}
		a.noticeAuth("Access denied (key passphrase), please try again.")
		p, perr := a.promptAuth("key passphrase (leave empty if none):", false)
		if perr != nil || (p == "" && attempt > 1) {
			return ConnectResult{Error: "cancelled", Kind: "cancelled"}
		}
		pass = p
	}
}

func (a *App) dialAndOpen(prof sessionstore.Profile, cred sshclient.Credential) ConnectResult {
	if a.kh == nil {
		return ConnectResult{Error: "known-hosts store unavailable", Kind: "internal"}
	}
	ctrl := app.New(prof, cred, a.kh, nil)
	if err := ctrl.Connect(100, 30); err != nil {
		var unk *sshclient.UnknownHostError
		if errors.As(err, &unk) {
			a.log("unknown host %s (%s)", unk.Addr, unk.Fingerprint)
			return ConnectResult{Error: unk.Error(), Kind: "unknown_host", Addr: unk.Addr, Fingerprint: unk.Fingerprint, KeyType: unk.KeyType}
		}
		var mm *sshclient.HostKeyMismatchError
		if errors.As(err, &mm) {
			a.log("host key MISMATCH %s", mm.Addr)
			return ConnectResult{Error: mm.Error(), Kind: "host_key_mismatch", Addr: mm.Addr, Fingerprint: mm.Actual, KeyType: mm.ActualType}
		}
		if sshclient.AuthFailed(err) {
			// Reported separately so Connect can re-prompt instead of giving up.
			a.log("auth failed %s@%s", prof.User, prof.Addr())
			return ConnectResult{Error: err.Error(), Kind: "auth"}
		}
		a.log("connect failed %s@%s: %v", prof.User, prof.Addr(), err)
		return ConnectResult{Error: err.Error(), Kind: "connect"}
	}
	id := newID()
	s := &appSession{ID: id, Ctrl: ctrl, Profile: prof, FilesPath: "/"}
	// The browser pane sits beside the terminal from the first second and the
	// shell starts in the remote home directory, so the pane begins there too:
	// a relative "cd logs" must resolve against where the user really is, not
	// against "/". SFTP is opened eagerly here (and cached by the controller),
	// which the always-visible pane needs anyway.
	if sc, err := ctrl.SFTP(); err == nil {
		s.FilesPath = sc.Home()
	}
	s.cancel = ctrl.SubscribeRaw(func(b []byte) {
		runtime.EventsEmit(a.ctx, "pty:"+id, string(b))
	})
	a.mu.Lock()
	a.sessions[id] = s
	a.order = append(a.order, id)
	a.mu.Unlock()
	// When the remote shell exits ("exit"/logout) or the link drops, tear the
	// session down and tell the UI, otherwise the tab stays on screen with a
	// dead transport behind it.
	var exitOnce sync.Once
	onExit := func() {
		exitOnce.Do(func() {
			a.CloseSession(id)
			runtime.EventsEmit(a.ctx, "session:exit", id)
		})
	}
	ctrl.OnExit(onExit)
	if ctrl.State() != app.StateConnected {
		// The shell had already gone by the time we could subscribe.
		onExit()
		return ConnectResult{Error: "session closed immediately after connect", Kind: "connect"}
	}
	a.log("connected %s@%s (sid=%s)", prof.User, prof.Addr(), id)
	return ConnectResult{SID: id}
}

// Trust records a host key fingerprint after user approval.
func (a *App) Trust(addr, fingerprint string) error {
	if a.kh == nil {
		return fmt.Errorf("known-hosts store unavailable")
	}
	a.log("trusted host %s (%s)", addr, fingerprint)
	return a.kh.Trust(addr, fingerprint)
}

// CloseSession tears down one SSH session, its tunnels, and its transfer queue.
func (a *App) CloseSession(sid string) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	var toClose []*tunnel
	for id, t := range a.tunnels {
		if t.SessionID == sid {
			toClose = append(toClose, t)
			delete(a.tunnels, id)
		}
	}
	if ok {
		delete(a.sessions, sid)
		for i, id := range a.order {
			if id == sid {
				a.order = append(a.order[:i], a.order[i+1:]...)
				break
			}
		}
	}
	a.mu.Unlock()
	for _, t := range toClose {
		_ = a.closeListenerOf(t)
	}
	if ok {
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.Ctrl.Close()
		a.log("closed session %s", sid)
	}
}

// SessionInfo is a UI-friendly summary of one session.
type SessionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	User      string `json:"user"`
	State     string `json:"state"`
	FilesPath string `json:"filesPath"`
}

// ListSessions returns all live sessions in creation order.
func (a *App) ListSessions() []SessionInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SessionInfo, 0, len(a.order))
	for _, id := range a.order {
		s, ok := a.sessions[id]
		if !ok {
			continue
		}
		out = append(out, SessionInfo{ID: s.ID, Name: s.Profile.Name, Host: s.Profile.Addr(), User: s.Profile.User, State: s.Ctrl.State().String(), FilesPath: s.FilesPath})
	}
	return out
}

// ---------- saved server profiles ----------

// ProfileInfo is a stored server (no secrets).
type ProfileInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Auth    string `json:"auth"`
	KeyPath string `json:"keyPath"`
}

// ListProfiles returns saved servers sorted by name.
func (a *App) ListProfiles() []ProfileInfo {
	if a.store == nil {
		return nil
	}
	list := a.store.List()
	out := make([]ProfileInfo, 0, len(list))
	for _, p := range list {
		out = append(out, ProfileInfo{ID: p.ID, Name: p.Name, Host: p.Host, Port: p.Port, Auth: string(p.Auth), KeyPath: p.KeyPath})
	}
	return out
}

// SaveProfile stores/updates a server profile. Login names and passwords are
// never stored; they are prompted in the terminal at connect time. Saving an
// existing name updates that row (PutNamed matches the name case-insensitively)
// instead of piling up duplicates.
func (a *App) SaveProfile(name, host string, port int, auth, keyPath string) error {
	if a.store == nil {
		return fmt.Errorf("profile store unavailable")
	}
	if name == "" {
		name = host
	}
	if port == 0 {
		port = 22
	}
	// A saved server is dialled again after a restart, so an out-of-range port
	// written here would only surface much later, in another session.
	if err := validPort("server", port); err != nil {
		a.log("profile save refused (%s): %v", name, err)
		return err
	}
	_, err := a.store.PutNamed(sessionstore.Profile{Name: name, Host: host, Port: port, Auth: sessionstore.AuthKind(auth), KeyPath: keyPath})
	if err != nil {
		a.log("profile save failed (%s): %v", name, err)
		return err
	}
	a.log("profile saved: %s (%s:%d, auth=%s)", name, host, port, auth)
	return nil
}

// DeleteProfile removes a saved server. A write failure is reported, not
// swallowed: the profile is still on disk and would reappear at the next start.
func (a *App) DeleteProfile(id string) error {
	if a.store == nil {
		return fmt.Errorf("profile store unavailable")
	}
	p, _ := a.store.Get(id)
	name := p.Name
	if name == "" {
		name = id
	}
	if err := a.store.Delete(id); err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return fmt.Errorf("no such profile")
		}
		a.log("profile delete failed (%s): %v", name, err)
		return fmt.Errorf("delete %s: %w", name, err)
	}
	a.log("profile deleted: %s", name)
	return nil
}

// ImportHosts creates saved server profiles from pasted text, one host per
// line. Accepted shapes: "host", "host:port", "name host", "name,host" or
// "name=host"; an ssh:// prefix is tolerated and # lines are comments. Names
// default to the address and PutNamed keeps re-imports idempotent. It returns
// how many profiles were created or updated.
func (a *App) ImportHosts(text string) int {
	if a.store == nil {
		return 0
	}
	count := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hostport := parseImportLine(line)
		hostport = strings.TrimPrefix(strings.TrimPrefix(hostport, "ssh://"), "//")
		host, port := hostport, 22
		if h, p, err := net.SplitHostPort(hostport); err == nil {
			host = h
			if v, cerr := strconv.Atoi(p); cerr == nil && v > 0 && v <= 65535 {
				port = v
			}
		}
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if name == "" {
			// Port variants must not collide in the name-keyed dedupe, but a
			// colon would read as a malformed hostname in the list - use a
			// label-style suffix instead: "linuxdb (3131)".
			name = host
			if port != 22 {
				name = fmt.Sprintf("%s (%d)", host, port)
			}
		}
		if _, err := a.store.PutNamed(sessionstore.Profile{Name: name, Host: host, Port: port, Auth: sessionstore.AuthPassword}); err != nil {
			a.log("import skipped %q: %v", line, err)
			continue
		}
		count++
	}
	a.log("imported %d host(s) from pasted text", count)
	return count
}

// parseImportLine splits an import line into an optional label and the host
// part. Only explicit separators count; a lone token is just a host.
func parseImportLine(line string) (name, hostport string) {
	for _, sep := range []string{",", "="} {
		if i := strings.Index(line, sep); i >= 0 {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
	}
	if fields := strings.Fields(line); len(fields) == 2 {
		return fields[0], fields[1]
	}
	return "", line
}

// PickHostFile opens the file picker from the Import dialog and returns the
// chosen file's text (capped at 1 MiB) so it lands in the preview box before
// anything is saved. Returns "" when the dialog is cancelled.
func (a *App) PickHostFile() (string, error) {
	p, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select hosts text file",
		Filters: []runtime.FileFilter{
			{DisplayName: "Text files (*.txt;*.list;*.csv;*.hosts)", Pattern: "*.txt;*.list;*.csv;*.hosts"},
			{DisplayName: "All files", Pattern: "*.*"},
		},
	})
	if err != nil || p == "" {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const maxImportFile = 1 << 20
	buf := make([]byte, maxImportFile)
	n, _ := io.ReadFull(f, buf)
	a.log("host file picked: %s (%d bytes)", p, n)
	return string(buf[:n]), nil
}

// ImportPuttySessions converts an installed PuTTY's saved sessions into
// Gravel profiles - the Import dialog's "From PuTTY" button, and the only
// way PuTTY sessions are pulled in: nothing is imported behind the user's
// back at startup. Re-importing updates rows by name and never duplicates;
// it does not delete profiles for sessions removed from PuTTY. The registry
// reading itself is platform-gated (see putty_import_windows.go).
func (a *App) ImportPuttySessions() (int, error) {
	if a.store == nil {
		return 0, fmt.Errorf("profile store unavailable")
	}
	n, err := a.importPuttySessions()
	if err != nil {
		a.log("putty import failed: %v", err)
		return n, err
	}
	a.log("putty import: %d session(s)", n)
	return n, nil
}

// ReorderProfiles persists the display order of saved servers as the user
// arranged them by dragging rows in the list.
func (a *App) ReorderProfiles(ids []string) error {
	if a.store == nil {
		return fmt.Errorf("profile store unavailable")
	}
	a.log("profiles reordered (%d rows)", len(ids))
	return a.store.Reorder(ids)
}

// ---------- PTY I/O ----------

// WriteInput forwards raw keystrokes to the remote PTY.
func (a *App) WriteInput(sid, data string) error {
	ctrl, err := a.controllerFor(sid)
	if err != nil {
		return err
	}
	return ctrl.WriteInput([]byte(data))
}

// ResizeSession updates the PTY window size.
func (a *App) ResizeSession(sid string, cols, rows int) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	a.mu.Unlock()
	if ok {
		s.Ctrl.Resize(cols, rows)
	}
}

// SessionScreen returns the visible screen a session's shell has produced so
// far. The frontend shares one terminal widget across all tabs, so switching
// tabs repaints from here instead of showing a blank until the remote types.
func (a *App) SessionScreen(sid string) (string, error) {
	ctrl, err := a.controllerFor(sid)
	if err != nil {
		return "", err
	}
	return ctrl.PlainText(), nil
}

// ---------- SFTP file manager ----------

// FileEntry is the frontend view of an SFTP listing row.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  string `json:"mode"`
	MTime string `json:"mtime"`
}

// ListFiles returns the entries of a remote directory.
func (a *App) ListFiles(sid, p string) ([]FileEntry, error) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	var start string
	if ok {
		start = s.FilesPath
	}
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no session %s", sid)
	}
	if p == "" {
		p = start
	}
	sc, err := s.Ctrl.SFTP()
	if err != nil {
		return nil, err
	}
	entries, err := sc.List(p)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	s.FilesPath = p
	a.mu.Unlock()
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, FileEntry{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size,
			Owner: e.Owner, Group: e.Group,
			Mode: e.Mode.String(), MTime: e.ModTime.Format("2006-01-02 15:04"),
		})
	}
	return out, nil
}

// FollowCwd inspects one submitted shell command line and, when it confidently
// changes the remote working directory, points the session's file browser at
// the new place. It returns that path, or "" when the browser should stay put:
// the line was not a cd, its target cannot be named with certainty, or the
// directory does not exist / cannot be read (a failed cd must not blank out a
// working listing).
func (a *App) FollowCwd(sid, line string) string {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	var cwd string
	if ok {
		cwd = s.FilesPath
	}
	a.mu.Unlock()
	if !ok {
		return ""
	}
	sc, err := s.Ctrl.SFTP()
	if err != nil {
		return ""
	}
	next, resolved := follow.Resolve(cwd, sc.Home(), line)
	if !resolved {
		// Never log the typed command line: shell history in a plaintext log
		// file is a security leak (paths may hold secrets, tokens, hostnames).
		// A miss is also the common case - every Enter arrives here.
		return ""
	}
	if fi, err := sc.Stat(next); err != nil || !fi.IsDir {
		// The failure path is not shown in the browser either, so it stays out
		// of the log as well; the error text can embed the attempted path.
		a.log("follow: cd target is not a usable directory (session %s)", sid)
		return ""
	}
	a.mu.Lock()
	s.FilesPath = next
	a.mu.Unlock()
	a.log("follow cd %s -> %s", sid, next)
	return next
}

// Mkdir creates a subdirectory under dir.
func (a *App) Mkdir(sid, dir, name string) error {
	sc, err := a.sftpFor(sid)
	if err != nil {
		return err
	}
	return sc.Mkdir(joinRemote(dir, name))
}

// NewFile creates an empty file inside dir. An existing name is reported as an
// error rather than overwritten.
func (a *App) NewFile(sid, dir, name string) error {
	sc, err := a.sftpFor(sid)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("file name required")
	}
	return sc.Touch(joinRemote(dir, name))
}

// Delete removes a file or directory (recursively).
func (a *App) Delete(sid, p string) error {
	sc, err := a.sftpFor(sid)
	if err != nil {
		return err
	}
	return sc.Remove(p)
}

// Rename replaces the basename of p with name.
func (a *App) Rename(sid, p, name string) error {
	sc, err := a.sftpFor(sid)
	if err != nil {
		return err
	}
	return sc.Rename(p, joinRemote(path.Dir(p), name))
}

// ---------- transfer queue ----------

// QueueItem is the frontend view of a transfer with progress/speed/ETA.
type QueueItem struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Direction   string `json:"direction"`
	Status      string `json:"status"`
	Progress    int    `json:"progress"`
	Speed       string `json:"speed"`
	ETA         string `json:"eta"`
	Transferred string `json:"transferred"`
	Total       string `json:"total"`
	Error       string `json:"error"`
}

// PickSSHKey opens a native file dialog so the user can choose a private key
// instead of typing its path. It returns "" when the dialog is cancelled. The
// "All files" filter is first so extension-less keys (id_rsa) are visible.
func (a *App) PickSSHKey() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select SSH private key",
		Filters: []runtime.FileFilter{
			{DisplayName: "All files", Pattern: "*.*"},
			{DisplayName: "SSH keys (*.pem;*.ppk;id_*)", Pattern: "*.pem;*.ppk;id_*"},
		},
	})
}

// Upload picks a local file and enqueues an upload to remoteDir.
func (a *App) Upload(sid, remoteDir string) error {
	local, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: "Select file to upload"})
	if err != nil || local == "" {
		return err
	}
	q, err := a.queueFor(sid)
	if err != nil {
		return err
	}
	remote := joinRemote(remoteDir, filepath.Base(local))
	q.AddUpload(local, remote)
	a.log("upload queued: %s -> %s", local, remote)
	return nil
}

// Download prompts for a local path and enqueues a download of remotePath.
func (a *App) Download(sid, remotePath string) error {
	dest, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{Title: "Save file as", DefaultFilename: path.Base(remotePath)})
	if err != nil || dest == "" {
		return err
	}
	q, err := a.queueFor(sid)
	if err != nil {
		return err
	}
	q.AddDownload(remotePath, dest)
	a.log("download queued: %s -> %s", remotePath, dest)
	return nil
}

// DownloadDir prompts for a local folder and enqueues every file under
// remotePath, mirroring the remote tree inside a subfolder named after it.
func (a *App) DownloadDir(sid, remotePath string) error {
	destRoot, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Save folder to"})
	if err != nil || destRoot == "" {
		return err
	}
	base := strings.TrimRight(remotePath, "/")
	if base == "" {
		return fmt.Errorf("downloading the filesystem root is not supported")
	}
	sc, err := a.sftpFor(sid)
	if err != nil {
		return err
	}
	paths, err := sc.Walk(base, maxDownloadFiles)
	if err != nil {
		return err
	}
	q, err := a.queueFor(sid)
	if err != nil {
		return err
	}
	for _, p := range paths {
		rel := strings.TrimPrefix(p, base+"/")
		dest := filepath.Join(destRoot, path.Base(base), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		q.AddDownload(p, dest)
	}
	a.log("download folder queued: %s (%d files) -> %s", base, len(paths), destRoot)
	return nil
}

// queueOf returns the transfer queue a session already has, or nil. Wails runs
// every bound method on its own goroutine, so the queue must be read under
// a.mu: the 600ms poll would otherwise race the click that installs the queue.
func (a *App) queueOf(sid string) *transfer.TransferQueue {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[sid]; ok {
		return s.queue
	}
	return nil
}

// QueueList returns the transfer queue for a session with computed metrics.
func (a *App) QueueList(sid string) ([]QueueItem, error) {
	q := a.queueOf(sid)
	if q == nil {
		return []QueueItem{}, nil
	}
	out := []QueueItem{}
	for _, it := range q.GetItems() {
		st, _ := time.Parse(time.RFC3339, it.StartTime)
		dir := "download"
		if it.Direction == transfer.Upload {
			dir = "upload"
		}
		out = append(out, QueueItem{
			ID: it.ID, Name: it.FileName, Direction: dir, Status: it.Status.String(),
			Progress: it.Progress(), Speed: transfer.FormatSpeed(it.Speed(st)), ETA: it.ETA(st),
			Transferred: transfer.FormatSize(it.TransferBytes), Total: transfer.FormatSize(it.TotalBytes), Error: it.ErrorMsg,
		})
	}
	return out, nil
}

// QueueCancel cancels a queued/in-progress transfer.
func (a *App) QueueCancel(sid string, id int) error {
	q := a.queueOf(sid)
	if q == nil {
		return fmt.Errorf("no session")
	}
	q.CancelItem(id)
	return nil
}

// QueuePause holds back a queued transfer, or stops a running one at the next
// chunk. The bytes already moved are kept, so QueueResume continues from that
// offset instead of restarting the file.
func (a *App) QueuePause(sid string, id int) error {
	q := a.queueOf(sid)
	if q == nil {
		return fmt.Errorf("no session")
	}
	q.PauseItem(id)
	return nil
}

// QueueResume re-queues a paused transfer from where it stopped.
func (a *App) QueueResume(sid string, id int) error {
	q := a.queueOf(sid)
	if q == nil {
		return fmt.Errorf("no session")
	}
	q.ResumeItem(id)
	return nil
}

// QueueClear removes completed/failed/cancelled items from the queue.
func (a *App) QueueClear(sid string) error {
	q := a.queueOf(sid)
	if q == nil {
		return nil
	}
	q.ClearCompleted()
	return nil
}

// queueFor lazily builds a transfer queue bound to the session's SFTP client.
func (a *App) queueFor(sid string) (*transfer.TransferQueue, error) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	var q *transfer.TransferQueue
	if ok {
		q = s.queue
	}
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no session %s", sid)
	}
	if q != nil {
		return q, nil
	}
	// Opening SFTP is a network round trip, so it must happen with a.mu released:
	// holding it across the handshake stalls every other bound call - the 5s
	// session poll, the queue poll, terminal I/O - behind a slow or dead server.
	sc, err := s.Ctrl.SFTP()
	if err != nil {
		return nil, err
	}
	built := transfer.NewTransferQueue(sc.Raw(), maxConcurrentTransfers)
	a.mu.Lock()
	if s.queue == nil {
		s.queue = built
	}
	// Another caller may have finished its handshake first; sharing that queue
	// keeps one progress list per session. Ours started nothing, so dropping it
	// releases no resources.
	q = s.queue
	a.mu.Unlock()
	return q, nil
}

// ---------- tunnels ----------

// TunnelInfo describes an active forward tunnel of either kind.
type TunnelInfo struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Kind       string `json:"kind"`
	Bind       string `json:"bind"`
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
	Remote     string `json:"remote"`
	Running    bool   `json:"running"`
}

// tunnelInfo projects a tunnel onto its UI row.
func tunnelInfo(t *tunnel) TunnelInfo {
	bind := "local"
	if t.bindAll {
		bind = "lan"
	}
	return TunnelInfo{
		ID: t.ID, SessionID: t.SessionID, Name: t.Name,
		Type: t.typeName(), Kind: t.kind(), Bind: bind,
		LocalPort: t.LocalPort, RemoteHost: t.RemoteHost, RemotePort: t.RemotePort,
		Remote: t.remoteAddr(), Running: t.running(),
	}
}

// bindIsLAN maps the frontend's "lan"/"local" bind value onto the flag.
func bindIsLAN(bind string) bool { return bind == "lan" }

// OpenTunnel starts a local port forward through the session's SSH connection.
// name labels the row; a blank name falls back to "local -> destination".
// bind is "local" (127.0.0.1, the default) or "lan" (0.0.0.0, reachable from
// other devices on the network).
func (a *App) OpenTunnel(sid, name string, localPort int, remoteHost string, remotePort int, bind string) (*TunnelInfo, error) {
	ctrl, err := a.controllerFor(sid)
	if err != nil {
		return nil, err
	}
	client := ctrl.SSHClient()
	if client == nil {
		return nil, fmt.Errorf("not connected")
	}
	if err := validPort("local", localPort); err != nil {
		return nil, err
	}
	if err := validPort("remote", remotePort); err != nil {
		return nil, err
	}
	t := &tunnel{ID: newID(), SessionID: sid, LocalPort: localPort, RemoteHost: remoteHost, RemotePort: remotePort, bindAll: bindIsLAN(bind), Name: strings.TrimSpace(name)}
	if t.Name == "" {
		t.Name = fmt.Sprintf("%d \u2192 %s", localPort, t.remoteAddr())
	}
	ln, err := net.Listen("tcp", t.localAddr())
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", t.localAddr(), err)
	}
	t.listener = ln
	a.mu.Lock()
	a.tunnels[t.ID] = t
	a.mu.Unlock()
	go a.serveTunnel(client, ln, t.remoteAddr())
	a.log("tunnel %s (%s) -> %s", t.localAddr(), t.Name, t.remoteAddr())
	info := tunnelInfo(t)
	return &info, nil
}

// OpenDynamicTunnel starts a SOCKS5 dynamic (-D) forward: the local port
// speaks SOCKS and every client request picks its own destination, dialled
// through the session's SSH connection. Like PuTTY's "Dynamic" it needs no
// remote host or port up front. bind is "local" or "lan" - a LAN bind turns
// the port into a proxy other devices (a phone, another PC) can use.
func (a *App) OpenDynamicTunnel(sid, name string, localPort int, bind string) (*TunnelInfo, error) {
	ctrl, err := a.controllerFor(sid)
	if err != nil {
		return nil, err
	}
	client := ctrl.SSHClient()
	if client == nil {
		return nil, fmt.Errorf("not connected")
	}
	if err := validPort("local", localPort); err != nil {
		return nil, err
	}
	t := &tunnel{ID: newID(), SessionID: sid, LocalPort: localPort, dynamic: true, bindAll: bindIsLAN(bind), Name: strings.TrimSpace(name)}
	if t.Name == "" {
		t.Name = fmt.Sprintf("SOCKS on %d", localPort)
	}
	ln, err := net.Listen("tcp", t.localAddr())
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", t.localAddr(), err)
	}
	t.listener = ln
	a.mu.Lock()
	a.tunnels[t.ID] = t
	a.mu.Unlock()
	go a.serveDynamic(client, ln)
	a.log("tunnel %s (%s) -> SOCKS dynamic", t.localAddr(), t.Name)
	info := tunnelInfo(t)
	return &info, nil
}

// StopTunnel closes the listener but keeps the definition, so the row stays in
// the table and can be started again with the same parameters.
func (a *App) StopTunnel(id string) error {
	a.mu.Lock()
	t, ok := a.tunnels[id]
	var ln net.Listener
	if ok {
		ln = t.listener
		t.listener = nil
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such tunnel")
	}
	if ln != nil {
		if err := ln.Close(); err != nil {
			a.log("stop tunnel %s: %v", id, err)
		}
	}
	a.log("tunnel stopped %s", id)
	return nil
}

// StartTunnel binds a listener for a stopped definition and serves it. It is a
// no-op when the forward is already running.
func (a *App) StartTunnel(id string) error {
	a.mu.Lock()
	t, ok := a.tunnels[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("no such tunnel")
	}
	if t.running() {
		a.mu.Unlock()
		return nil
	}
	sid, local, remote := t.SessionID, t.localAddr(), t.remoteAddr()
	a.mu.Unlock()

	ctrl, err := a.controllerFor(sid)
	if err != nil {
		return err
	}
	client := ctrl.SSHClient()
	if client == nil {
		return fmt.Errorf("not connected")
	}
	ln, err := net.Listen("tcp", local)
	if err != nil {
		return fmt.Errorf("listen %s: %w", local, err)
	}
	a.mu.Lock()
	if a.tunnels[id] != t {
		// The row was closed while we were binding. Hand the port straight back:
		// serving on a definition the UI no longer lists would leak a listener
		// that nothing can stop later.
		a.mu.Unlock()
		_ = ln.Close()
		return fmt.Errorf("no such tunnel")
	}
	if t.running() {
		// Another call won the race while we were binding.
		a.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	t.listener = ln
	a.mu.Unlock()
	if t.dynamic {
		go a.serveDynamic(client, ln)
		a.log("tunnel started %s (SOCKS)", local)
		return nil
	}
	go a.serveTunnel(client, ln, remote)
	a.log("tunnel started %s -> %s", local, remote)
	return nil
}

// UpdateTunnel replaces a tunnel's parameters in place, keeping its row. The
// forward is stopped, the new values applied and it is started again; if the new
// settings cannot be bound the definition survives with them and the row simply
// reads "stopped", so the fix is a click rather than a retyped tunnel.
func (a *App) UpdateTunnel(id, name string, localPort int, remoteHost string, remotePort int, bind string) (*TunnelInfo, error) {
	// Validate before anything is torn down: an edit with an out-of-range port
	// must not stop a forward that is currently carrying traffic and then fail.
	a.mu.Lock()
	t, ok := a.tunnels[id]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such tunnel")
	}
	if err := validPort("local", localPort); err != nil {
		return nil, err
	}
	if !t.dynamic {
		if err := validPort("remote", remotePort); err != nil {
			return nil, err
		}
	}
	if err := a.StopTunnel(id); err != nil {
		return nil, err
	}
	a.mu.Lock()
	t, ok = a.tunnels[id]
	if !ok {
		a.mu.Unlock()
		return nil, fmt.Errorf("no such tunnel")
	}
	if n := strings.TrimSpace(name); n != "" {
		t.Name = n
	}
	t.LocalPort = localPort
	t.bindAll = bindIsLAN(bind)
	// A dynamic forward dials no fixed target, so the remote fields stay unused.
	if !t.dynamic {
		t.RemoteHost, t.RemotePort = remoteHost, remotePort
	}
	info := tunnelInfo(t)
	a.mu.Unlock()

	if err := a.StartTunnel(id); err != nil {
		a.log("update tunnel %s failed: %v", id, err)
		return nil, err
	}
	a.mu.Lock()
	info = tunnelInfo(t)
	a.mu.Unlock()
	return &info, nil
}

// tunnelDialTimeout bounds the SSH-side dial of a forwarded connection. A -L or
// -D forward can be bound to the LAN, and a target that swallows packets instead
// of refusing them would otherwise leave a goroutine parked in the handshake for
// as long as the client keeps its socket open. A var so the tests do not have to
// wait out the real budget.
var tunnelDialTimeout = 15 * time.Second

// sshDialer is the part of *ssh.Client the forwarders use. Naming it lets the
// dial-timeout behaviour be tested against a stub that never answers.
type sshDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// dialBounded opens a connection to target through the SSH link and gives up
// after tunnelDialTimeout. Client.DialContext closes a connection that completes
// late, so a slow server cannot smuggle one past the deadline.
func dialBounded(d sshDialer, network, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tunnelDialTimeout)
	defer cancel()
	return d.DialContext(ctx, network, target)
}

func (a *App) serveTunnel(dialer sshDialer, ln net.Listener, target string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.forward(dialer, conn, target)
	}
}

// serveDynamic accepts SOCKS5 clients on a dynamic forward's listener; each
// connection performs its own handshake and target dial over the SSH link.
func (a *App) serveDynamic(dialer sshDialer, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			if err := socks.Serve(conn, func(network, addr string) (net.Conn, error) {
				return dialBounded(dialer, network, addr)
			}); err != nil {
				// A client hanging up mid-handshake is normal; keep it out of
				// the log so only real relay problems would stand out.
				_ = err
			}
		}()
	}
}

func (a *App) forward(dialer sshDialer, conn net.Conn, target string) {
	defer conn.Close()
	remote, err := dialBounded(dialer, "tcp", target)
	if err != nil {
		return
	}
	defer remote.Close()
	go func() { _, _ = io.Copy(remote, conn); remote.Close() }()
	_, _ = io.Copy(conn, remote)
}

// CloseTunnel removes a tunnel definition entirely. Use StopTunnel to keep the
// row for later use.
func (a *App) CloseTunnel(id string) error {
	a.mu.Lock()
	t, ok := a.tunnels[id]
	if ok {
		delete(a.tunnels, id)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such tunnel")
	}
	return a.closeListenerOf(t)
}

// ListTunnels returns every tunnel definition, running or stopped.
func (a *App) ListTunnels() []TunnelInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]TunnelInfo, 0, len(a.tunnels))
	for _, t := range a.tunnels {
		out = append(out, tunnelInfo(t))
	}
	return out
}

// StopAllTunnels stops every running forward belonging to a session, keeping the
// definitions, and reports how many it stopped.
func (a *App) StopAllTunnels(sid string) int {
	a.mu.Lock()
	var ids []string
	for id, t := range a.tunnels {
		if t.SessionID == sid && t.running() {
			ids = append(ids, id)
		}
	}
	a.mu.Unlock()
	for _, id := range ids {
		if err := a.StopTunnel(id); err != nil {
			a.log("stop tunnel %s: %v", id, err)
		}
	}
	if len(ids) > 0 {
		a.log("stopped %d tunnel(s) for session %s", len(ids), sid)
	}
	return len(ids)
}

// StartAllTunnels starts every stopped forward belonging to a session and
// reports how many came up. Ones that cannot bind stay stopped and are logged.
func (a *App) StartAllTunnels(sid string) int {
	a.mu.Lock()
	var ids []string
	for id, t := range a.tunnels {
		if t.SessionID == sid && !t.running() {
			ids = append(ids, id)
		}
	}
	a.mu.Unlock()
	started := 0
	for _, id := range ids {
		if err := a.StartTunnel(id); err != nil {
			a.log("start tunnel %s: %v", id, err)
			continue
		}
		started++
	}
	if started > 0 {
		a.log("started %d tunnel(s) for session %s", started, sid)
	}
	return started
}

// ---------- helpers ----------

func (a *App) controllerFor(sid string) (*app.Controller, error) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	a.mu.Unlock()
	if !ok || s.Ctrl == nil {
		return nil, fmt.Errorf("no session %s", sid)
	}
	return s.Ctrl, nil
}

func (a *App) sftpFor(sid string) (*sftpclient.Client, error) {
	a.mu.Lock()
	s, ok := a.sessions[sid]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no session %s", sid)
	}
	return s.Ctrl.SFTP()
}

func joinRemote(dir, name string) string {
	if dir == "/" || dir == "" {
		return "/" + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
