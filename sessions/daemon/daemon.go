package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/obot-platform/discobox/id"
	sessions "github.com/obot-platform/discobox/sessions"
	sessionapigen "github.com/obot-platform/discobox/sessions/api/gen"
	supervisorapigen "github.com/obot-platform/discobox/sessions/api/supervisorgen"
	"github.com/obot-platform/discobox/sessions/models"
	"github.com/obot-platform/discobox/sessions/store"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const defaultIdleTimeout = 5 * time.Minute

type Config struct {
	SessionID     string
	RepoRoot      string
	DBPath        string
	SocketPath    string
	RuntimePath   string
	SupervisorDir string
	ConfigPath    string
	Version       int64
	IdleTimeout   time.Duration
}

func Run(ctx context.Context, cfg Config) error {
	r, err := newRuntime(cfg)
	if err != nil {
		return err
	}
	return r.run(ctx)
}

type runtimeState struct {
	cfg      Config
	agents   []sessions.Agent
	store    *store.Store
	server   *http.Server
	listener net.Listener

	ctx    context.Context
	cancel context.CancelFunc

	mu             sync.Mutex
	lastActivity   time.Time
	activeRequests int64
	activeAttaches int64
}

type attachWriter struct {
	mu        sync.Mutex
	w         *bufio.ReadWriter
	done      chan struct{}
	closeOnce sync.Once
}

func newRuntime(cfg Config) (*runtimeState, error) {
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if cfg.RepoRoot == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	root, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, err
	}
	cfg.RepoRoot = root
	if cfg.SocketPath == "" {
		return nil, fmt.Errorf("socket path is required")
	}
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("db path is required")
	}
	if cfg.RuntimePath == "" {
		cfg.RuntimePath = filepath.Join(filepath.Dir(cfg.SocketPath), "runtime.json")
	}
	if cfg.SupervisorDir == "" {
		cfg.SupervisorDir = filepath.Join(filepath.Dir(cfg.SocketPath), "supervisors")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	agents, err := loadAgents(cfg)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(context.Background(), store.Options{Path: cfg.DBPath, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &runtimeState{
		cfg:          cfg,
		agents:       agents,
		store:        st,
		ctx:          ctx,
		cancel:       cancel,
		lastActivity: time.Now().UTC(),
	}, nil
}

func loadAgents(cfg Config) ([]sessions.Agent, error) {
	config, err := readConfig(cfg)
	if err != nil {
		return nil, err
	}
	return sessions.MergeConfig(sessions.DefaultAgents(), config)
}

func readConfig(cfg Config) (sessions.Config, error) {
	candidates := []string{}
	if cfg.ConfigPath != "" {
		candidates = append(candidates, cfg.ConfigPath)
	}
	if env := os.Getenv("DISCOBOX_SESSIONS_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	candidates = append(candidates, filepath.Join(cfg.RepoRoot, ".discobox", "sessions.json"))
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "discobox", "sessions.json"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "discobox", "sessions.json"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return sessions.Config{}, fmt.Errorf("read session config %s: %w", path, err)
		}
		var cfg sessions.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return sessions.Config{}, fmt.Errorf("parse session config %s: %w", path, err)
		}
		return cfg, nil
	}
	return sessions.Config{}, nil
}

func (r *runtimeState) run(parent context.Context) (err error) {
	defer func() {
		r.cancel()
		if r.server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = r.server.Shutdown(ctx)
			cancel()
		}
		if r.listener != nil {
			_ = r.listener.Close()
		}
		_ = os.Remove(r.cfg.SocketPath)
		if r.store != nil {
			_ = r.store.Close()
		}
	}()
	if err := r.startServer(); err != nil {
		return err
	}
	if err := r.writeRuntimeMetadata(); err != nil {
		return err
	}
	go r.serve()
	go r.idleLoop()
	select {
	case <-parent.Done():
		return parent.Err()
	case <-r.ctx.Done():
		return nil
	}
}

func (r *runtimeState) startServer() error {
	if err := os.MkdirAll(filepath.Dir(r.cfg.SocketPath), 0o755); err != nil {
		return err
	}
	if err := prepareSocketPath(r.cfg.SocketPath); err != nil {
		return err
	}
	ln, err := net.Listen("unix", r.cfg.SocketPath)
	if err != nil {
		return err
	}
	routes, err := r.generatedRoutes()
	if err != nil {
		_ = ln.Close()
		return err
	}
	r.listener = ln
	r.server = &http.Server{Handler: r.withRequestTracking(routes)}
	return nil
}

func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	return os.Remove(path)
}

func (r *runtimeState) serve() {
	if err := r.server.Serve(r.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		r.cancel()
	}
}

func (r *runtimeState) generatedRoutes() (http.Handler, error) {
	generated, err := sessionapigen.NewServer(
		&generatedHandler{r: r},
		sessionapigen.WithNotFound(func(w http.ResponseWriter, req *http.Request) { http.NotFound(w, req) }),
		sessionapigen.WithMethodNotAllowed(func(w http.ResponseWriter, req *http.Request, _ string) {
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		}),
	)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			if sessionID, ok := attachSessionID(req.URL.Path); ok {
				r.proxyAttach(w, sessionID)
				return
			}
		}
		generated.ServeHTTP(w, req)
	}), nil
}

func attachSessionID(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/sessions/")
	if rest == path {
		return "", false
	}
	sessionID, action, ok := strings.Cut(rest, "/")
	return sessionID, ok && action == "attach" && sessionID != ""
}

func (r *runtimeState) withRequestTracking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt64(&r.activeRequests, 1)
		r.touch()
		defer func() {
			atomic.AddInt64(&r.activeRequests, -1)
			r.touch()
		}()
		next.ServeHTTP(w, req)
	})
}

func (r *runtimeState) touch() {
	r.mu.Lock()
	r.lastActivity = time.Now().UTC()
	r.mu.Unlock()
}

func (r *runtimeState) create(_ context.Context, req sessions.CreateRequest) (sessions.Session, error) {
	agent, ok := sessions.AgentByID(r.agents, req.AgentID)
	if !ok {
		return sessions.Session{}, fmt.Errorf("unsupported agent %q", req.AgentID)
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = r.cfg.RepoRoot
	}
	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return sessions.Session{}, err
	}
	if info, err := os.Stat(absWorkdir); err != nil {
		return sessions.Session{}, err
	} else if !info.IsDir() {
		return sessions.Session{}, fmt.Errorf("workdir %s is not a directory", absWorkdir)
	}
	command := append([]string(nil), agent.Command...)
	command = append(command, req.Args...)
	if len(command) == 0 {
		return sessions.Session{}, fmt.Errorf("agent %q has empty command", agent.ID)
	}
	sessionID, err := id.New()
	if err != nil {
		return sessions.Session{}, fmt.Errorf("generate session id: %w", err)
	}
	supervisorToken, err := newSupervisorToken()
	if err != nil {
		return sessions.Session{}, fmt.Errorf("generate supervisor token: %w", err)
	}
	now := time.Now().UTC()
	supervisorName := shortSessionFileName(sessionID)
	supervisorSocket := filepath.Join(r.cfg.SupervisorDir, supervisorName+".sock")
	runtimePath := filepath.Join(r.cfg.SupervisorDir, supervisorName+".json")
	row := &models.CodingSession{
		ID:               sessionID,
		AgentID:          agent.ID,
		Command:          store.EncodeCommand(command),
		Workdir:          absWorkdir,
		Status:           string(models.StatusStarting),
		SupervisorSocket: supervisorSocket,
		SupervisorToken:  supervisorToken,
		RuntimePath:      runtimePath,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := r.store.CreateSession(context.Background(), row); err != nil {
		return sessions.Session{}, err
	}
	supervisorPID, err := r.startDetachedSupervisor(store.RowToSession(*row), req.Rows, req.Cols)
	if err != nil {
		_ = r.store.MarkLost(context.Background(), row, err.Error())
		return sessions.Session{}, err
	}
	row.SupervisorPID = supervisorPID
	if err := r.waitSupervisorReady(context.Background(), row); err != nil {
		_ = r.store.MarkLost(context.Background(), row, err.Error())
		return sessions.Session{}, err
	}
	status, err := supervisorStatus(context.Background(), row.SupervisorSocket, row.SupervisorToken)
	if err != nil {
		if exit, ok, readErr := store.ReadRuntimeExit(row.RuntimePath); readErr != nil {
			return sessions.Session{}, readErr
		} else if ok && exit.ExitedAt != nil {
			store.ApplyRuntimeExit(row, exit)
			if err := r.store.UpdateSession(context.Background(), row); err != nil {
				return sessions.Session{}, err
			}
			return store.RowToSession(*row), nil
		}
		_ = r.store.MarkLost(context.Background(), row, err.Error())
		return sessions.Session{}, err
	}
	row.PID = status.PID
	row.Status = string(models.StatusRunning)
	if err := r.store.UpdateSession(context.Background(), row); err != nil {
		return sessions.Session{}, err
	}
	r.mu.Lock()
	r.lastActivity = now
	r.mu.Unlock()
	return store.RowToSession(*row), nil
}

func (r *runtimeState) startDetachedSupervisor(session sessions.Session, rows, cols uint16) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	commandJSON, err := json.Marshal(session.Command)
	if err != nil {
		return 0, err
	}
	args := []string{"--session-id", r.cfg.SessionID, "--repo-root", r.cfg.RepoRoot, "daemon", "supervisor",
		"--coding-session-id", session.ID,
		"--agent-id", session.AgentID,
		"--workdir", session.Workdir,
		"--socket", filepath.Join(r.cfg.SupervisorDir, shortSessionFileName(session.ID)+".sock"),
		"--runtime", filepath.Join(r.cfg.SupervisorDir, shortSessionFileName(session.ID)+".json"),
		"--rows", strconv.Itoa(int(rows)),
		"--cols", strconv.Itoa(int(cols)),
		"--command", base64.StdEncoding.EncodeToString(commandJSON),
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	row, err := r.store.GetSession(context.Background(), session.ID)
	if err != nil {
		return 0, err
	}
	cmd.Env = append(os.Environ(), "DISCOBOX_SESSION_ID="+r.cfg.SessionID, "DISCOBOX_SUPERVISOR_TOKEN="+row.SupervisorToken)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func newSupervisorToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(token[:]), nil
}

func shortSessionFileName(sessionID string) string {
	if len(sessionID) <= 12 {
		return safeRuntimeName(sessionID)
	}
	return safeRuntimeName(sessionID[len(sessionID)-12:])
}

func safeRuntimeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "session"
	}
	return out
}

func (r *runtimeState) waitSupervisorReady(ctx context.Context, row *models.CodingSession) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := supervisorStatus(ctx, row.SupervisorSocket, row.SupervisorToken); err == nil {
			return nil
		}
		if exit, ok, err := store.ReadRuntimeExit(row.RuntimePath); err != nil {
			return err
		} else if ok && exit.ExitedAt != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("supervisor did not become ready at %s", row.SupervisorSocket)
}

func (r *runtimeState) listSessions(ctx context.Context) ([]sessions.Session, error) {
	rows, err := r.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sessions.Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.RowToSession(row))
	}
	return out, nil
}

func (r *runtimeState) reconcileSessions(ctx context.Context) error {
	rows, err := r.store.ListSessions(ctx)
	if err != nil {
		return err
	}
	for i := range rows {
		row := &rows[i]
		if row.Status == string(models.StatusTerminated) || row.Status == string(models.StatusLost) {
			continue
		}
		if exit, ok, err := store.ReadRuntimeExit(row.RuntimePath); err != nil {
			return err
		} else if ok && exit.ExitedAt != nil {
			store.ApplyRuntimeExit(row, exit)
			if err := r.store.UpdateSession(ctx, row); err != nil {
				return err
			}
			continue
		}
		status, err := supervisorStatus(ctx, row.SupervisorSocket, row.SupervisorToken)
		if err != nil {
			if supervisorProcessAlive(row.SupervisorPID) {
				continue
			}
			if err := r.store.MarkLost(ctx, row, "supervisor is not running"); err != nil {
				return err
			}
			continue
		}
		row.PID = status.PID
		row.Status = string(models.StatusRunning)
		if err := r.store.UpdateSession(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func supervisorProcessAlive(pid int) bool {
	if !processExists(pid) {
		return false
	}
	if runtime.GOOS != "linux" {
		return true
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return true
	}
	return bytes.Contains(data, []byte("\x00daemon\x00supervisor\x00"))
}

func supervisorStatus(ctx context.Context, socket string, token string) (sessions.Session, error) {
	client, err := newSupervisorClient(socket, token)
	if err != nil {
		return sessions.Session{}, err
	}
	res, err := client.SupervisorStatus(ctx)
	if err != nil {
		return sessions.Session{}, err
	}
	out, ok := res.(*supervisorapigen.Session)
	if !ok {
		return sessions.Session{}, supervisorAPIError(res)
	}
	var session sessions.Session
	if err := convertGenerated(out, &session); err != nil {
		return sessions.Session{}, err
	}
	return session, nil
}

func newSupervisorClient(socket string, token string) (*supervisorapigen.Client, error) {
	httpClient := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	return supervisorapigen.NewClient("http://unix", supervisorSecuritySource{token: token}, supervisorapigen.WithClient(httpClient))
}

func supervisorAPIError(res any) error {
	switch out := res.(type) {
	case *supervisorapigen.ErrorResponse:
		return fmt.Errorf("supervisor returned error: %s", out.Error)
	case *supervisorapigen.SupervisorResizeBadRequest:
		return fmt.Errorf("supervisor returned error: %s", out.Error)
	case *supervisorapigen.SupervisorResizeUnauthorized:
		return fmt.Errorf("supervisor returned error: %s", out.Error)
	case *supervisorapigen.SupervisorSignalBadRequest:
		return fmt.Errorf("supervisor returned error: %s", out.Error)
	case *supervisorapigen.SupervisorSignalUnauthorized:
		return fmt.Errorf("supervisor returned error: %s", out.Error)
	default:
		return fmt.Errorf("supervisor returned unexpected response %T", res)
	}
}

func supervisorResize(ctx context.Context, socket string, token string, req sessions.ResizeRequest) error {
	client, err := newSupervisorClient(socket, token)
	if err != nil {
		return err
	}
	res, err := client.SupervisorResize(ctx, &supervisorapigen.ResizeRequest{Cols: int(req.Cols), Rows: int(req.Rows)})
	if err != nil {
		return err
	}
	if _, ok := res.(*supervisorapigen.ActionResponse); !ok {
		return supervisorAPIError(res)
	}
	return nil
}

func supervisorSignal(ctx context.Context, socket string, token string, signal string) error {
	client, err := newSupervisorClient(socket, token)
	if err != nil {
		return err
	}
	res, err := client.SupervisorSignal(ctx, &supervisorapigen.SignalRequest{Signal: signal})
	if err != nil {
		return err
	}
	if _, ok := res.(*supervisorapigen.ActionResponse); !ok {
		return supervisorAPIError(res)
	}
	return nil
}

func (r *runtimeState) proxyAttach(w http.ResponseWriter, sessionID string) {
	row, err := r.store.GetSession(context.Background(), sessionID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	proxyHijacked(w, row.SupervisorSocket, row.SupervisorToken, "/attach")
}

func (r *runtimeState) supervisorResize(ctx context.Context, sessionID string, req sessions.ResizeRequest) error {
	row, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	return supervisorResize(ctx, row.SupervisorSocket, row.SupervisorToken, req)
}

func (r *runtimeState) supervisorSignal(ctx context.Context, sessionID string, signal string) error {
	row, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	return supervisorSignal(ctx, row.SupervisorSocket, row.SupervisorToken, signal)
}

func proxyHijacked(w http.ResponseWriter, socket, token, path string) {
	clientConn, clientRW, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer clientConn.Close()
	supervisorConn, err := net.Dial("unix", socket)
	if err != nil {
		_, _ = clientRW.WriteString("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		_ = clientRW.Flush()
		return
	}
	defer supervisorConn.Close()
	req, err := http.NewRequest(http.MethodPost, "http://unix"+path, nil)
	if err != nil {
		return
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-session")
	req.Header.Set("Authorization", "Bearer "+token)
	if err := req.Write(supervisorConn); err != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(supervisorConn, clientRW)
		if conn, ok := supervisorConn.(*net.UnixConn); ok {
			_ = conn.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, supervisorConn)
		if conn, ok := clientConn.(*net.UnixConn); ok {
			_ = conn.CloseWrite()
		}
	}()
	wg.Wait()
}

func (a *attachWriter) close() {
	a.closeOnce.Do(func() {
		close(a.done)
	})
}

func (a *attachWriter) writeFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := sessions.WriteFrame(a.w, typ, payload); err != nil {
		a.close()
		return err
	}
	if err := a.w.Flush(); err != nil {
		a.close()
		return err
	}
	return nil
}

func (a *attachWriter) Write(payload []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, err := a.w.Write(payload)
	if err != nil {
		a.close()
		return n, err
	}
	if err := a.w.Flush(); err != nil {
		a.close()
		return n, err
	}
	return n, nil
}

func signalProcess(process *os.Process, name string) error {
	if process == nil {
		return fmt.Errorf("process is unavailable")
	}
	sig, err := parseSignal(name)
	if err != nil {
		return err
	}
	return process.Signal(sig)
}

func parseSignal(name string) (os.Signal, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(name))
	if trimmed == "" {
		return nil, fmt.Errorf("signal is required")
	}
	if num, err := strconv.Atoi(trimmed); err == nil {
		return syscall.Signal(num), nil
	}
	trimmed = strings.TrimPrefix(trimmed, "SIG")
	switch trimmed {
	case "INT":
		return syscall.SIGINT, nil
	case "TERM":
		return syscall.SIGTERM, nil
	case "KILL":
		return syscall.SIGKILL, nil
	case "HUP":
		return syscall.SIGHUP, nil
	case "QUIT":
		return syscall.SIGQUIT, nil
	case "USR1":
		return syscall.SIGUSR1, nil
	case "USR2":
		return syscall.SIGUSR2, nil
	default:
		return nil, fmt.Errorf("unsupported signal %q", name)
	}
}

func (r *runtimeState) idleLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			if r.shouldIdleShutdown() {
				r.cancel()
				return
			}
		}
	}
}

func (r *runtimeState) shouldIdleShutdown() bool {
	if atomic.LoadInt64(&r.activeRequests) > 0 || atomic.LoadInt64(&r.activeAttaches) > 0 {
		return false
	}
	r.mu.Lock()
	last := r.lastActivity
	timeout := r.cfg.IdleTimeout
	r.mu.Unlock()
	return time.Since(last) >= timeout
}

func (r *runtimeState) writeRuntimeMetadata() error {
	if r.cfg.RuntimePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.cfg.RuntimePath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]any{
		"sessionId": r.cfg.SessionID,
		"repoRoot":  r.cfg.RepoRoot,
		"socket":    r.cfg.SocketPath,
		"pid":       os.Getpid(),
		"version":   r.cfg.Version,
		"startedAt": time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(r.cfg.RuntimePath, data, 0o600)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
