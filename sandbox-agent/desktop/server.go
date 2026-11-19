package desktop

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

//go:embed web
var webFS embed.FS

// Defaults for everything the service is pointed at. Each is a path this
// image's own Dockerfile installs, so the service needs no configuration to
// work and the flags exist for tests and for running it outside the image.
const (
	DefaultAddr = "127.0.0.1:6900"
	// DefaultStreamAddr is where voxl listens, on loopback and behind this
	// service. The agent's port watcher excludes it by number, so it is never
	// listed as one of the sandbox's ports or probed: it is the desktop's own
	// plumbing, reached only through /api/stream. A fixed port rather than a
	// free one picked at start, because a port that moves cannot be excluded
	// ahead of time and would be listed and probed while voxl runs; in the
	// 170xx range beside the agent's other listeners, where a sandbox's own
	// service is unlikely to be, since the exclusion hides whatever holds it.
	DefaultStreamAddr = "127.0.0.1:17063"
	// DefaultVoxlDir is the desktop feature's voxl: the binary, and the
	// browser client and decode worker built from the same source under web/.
	DefaultVoxlDir     = "/usr/local/lib/discobox/voxl"
	DefaultBrandDir    = "/usr/local/share/discobox/brand"
	DefaultFeedbackDir = "~/.discobox/desktop-feedback"
	DefaultScaleEnvDir = "~/" + ScaleEnvDir

	// maxScreenshot bounds a posted capture. A full 3840x2400 PNG of a
	// photographic desktop lands well under this; anything larger is not a
	// screenshot of a region.
	maxScreenshot = 12 << 20
	// Two of them ride in one capture — the crop and the whole desktop — plus
	// the words about them.
	//
	// Sized in *encoded* bytes, because that is what the cap is applied to: the
	// body is JSON carrying both PNGs as base64 data URLs, which is 4/3 of
	// their decoded size. Sizing it in decoded bytes made two captures at the
	// documented limit fail as "unexpected EOF" — the LimitReader truncated the
	// body mid-string, and the page showed the parse error, which says nothing
	// about size.
	// The data URL's prefix and base64's padding are inside the 1MiB slack.
	maxBody = 2*(maxScreenshot*4/3) + (1 << 20)
)

// Config is what the service needs to run.
type Config struct {
	// Addr is bound when no listener is passed in, which is how this runs
	// outside systemd.
	Addr string
	// FeedbackDir holds the annotation record. A leading ~ is expanded against
	// the user this process runs as.
	FeedbackDir string
	// Display is the X display to size.
	Display string
	// ScaleEnvDir holds the toolkit environment file that carries the desktop
	// scale to programs launched afterwards. See scale.go.
	ScaleEnvDir string
	// VSCodeArgv is VS Code's argv.json, which carries the scale to VS Code.
	// See vscode.go.
	VSCodeArgv string
	// StreamAddr is the loopback address voxl is started on. The page reaches
	// it only through /api/stream, so it has one origin for its markup and
	// its pixels.
	StreamAddr string
	// VoxlDir holds voxl's binary and, under web/, the client and worker
	// served as /voxl/.
	VoxlDir string
	// BrandDir is served read-only as /brand/.
	BrandDir string
	// AllowedHosts are host names the desktop answers for besides localhost
	// and loopback addresses: the name a forwarding layer reaches it under.
	// See access.go.
	AllowedHosts []string
}

func (c *Config) applyDefaults() {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.FeedbackDir == "" {
		c.FeedbackDir = DefaultFeedbackDir
	}
	if c.Display == "" {
		c.Display = ":0"
	}
	if c.ScaleEnvDir == "" {
		c.ScaleEnvDir = DefaultScaleEnvDir
	}
	if c.VSCodeArgv == "" {
		c.VSCodeArgv = DefaultVSCodeArgv
	}
	if c.StreamAddr == "" {
		c.StreamAddr = DefaultStreamAddr
	}
	if c.VoxlDir == "" {
		c.VoxlDir = DefaultVoxlDir
	}
	if c.BrandDir == "" {
		c.BrandDir = DefaultBrandDir
	}
}

// Server is the desktop viewer: a branded page, the voxl client it loads, a
// same-origin path through to voxl's stream, and the annotation record the page
// writes into.
type Server struct {
	log      *slog.Logger
	store    *Store
	display  *Display
	streamer *Streamer
	static   fs.FS
	voxl     http.Handler
	brand    http.Handler
	prompt   string
	access   accessPolicy
	handler  http.Handler

	// releaseOnce guards the readiness notification, which lets the desktop
	// session start. It fires exactly once, whatever gets there first.
	releaseOnce sync.Once
}

// PrepareSession makes the X server agree with the scale the desktop session is
// about to start at. It is xfce4-session@.service's ExecStartPre, run as the
// session's user with its display and bus; see Display.prepareSession.
func PrepareSession(ctx context.Context, cfg Config) (int, error) {
	cfg.applyDefaults()
	envDir, err := expandHome(cfg.ScaleEnvDir)
	if err != nil {
		return 0, err
	}
	vscodeArgv, err := expandHome(cfg.VSCodeArgv)
	if err != nil {
		return 0, err
	}
	display := NewDisplay(cfg.Display)
	display.EnvDir = envDir
	display.VSCodeArgv = vscodeArgv
	scale, err := display.prepareSession(ctx)
	// A session started by a program talking to :0, with no viewer to have
	// adopted a scale, still has to open VS Code at the scale it is drawn at.
	display.writeVSCodeScale(scale)
	return scale, err
}

// New builds the server. It does not bind anything.
func New(log *slog.Logger, cfg Config) (*Server, error) {
	cfg.applyDefaults()
	dir, err := expandHome(cfg.FeedbackDir)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(dir)
	if err != nil {
		return nil, err
	}
	envDir, err := expandHome(cfg.ScaleEnvDir)
	if err != nil {
		return nil, err
	}
	if _, _, err := splitHostPort(cfg.StreamAddr); err != nil {
		return nil, err
	}
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, fmt.Errorf("desktop: open embedded assets: %w", err)
	}
	vscodeArgv, err := expandHome(cfg.VSCodeArgv)
	if err != nil {
		return nil, err
	}
	display := NewDisplay(cfg.Display)
	display.EnvDir = envDir
	display.VSCodeArgv = vscodeArgv
	display.Log = log
	s := &Server{
		log:     log,
		store:   store,
		display: display,
		streamer: &Streamer{
			Binary:  filepath.Join(cfg.VoxlDir, "voxl"),
			Addr:    cfg.StreamAddr,
			Display: cfg.Display,
			Log:     log,
		},
		static: static,
		voxl:   revalidated(http.StripPrefix("/voxl/", http.FileServer(http.Dir(filepath.Join(cfg.VoxlDir, "web"))))),
		brand:  http.StripPrefix("/brand/", http.FileServer(http.Dir(cfg.BrandDir))),
		prompt: prompt(store.MarkdownPath()),
		access: newAccessPolicy(cfg.AllowedHosts),
	}
	s.handler = s.routes()
	return s, nil
}

// Prompt is the line the page offers to copy: what to say to the agent to have
// it act on what was drawn.
func (s *Server) Prompt() string { return s.prompt }

func prompt(markdownPath string) string {
	return fmt.Sprintf("Read %s and resolve every unticked item, looking at the screenshot "+
		"beside each one. Reply under each item with what you changed, or why you did not. "+
		"Leave the checkboxes alone — they are for whoever asked.", markdownPath)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.Handle("GET /app.css", http.FileServerFS(s.static))
	mux.Handle("GET /app.js", http.FileServerFS(s.static))
	// TestEveryModuleThePageImportsIsServed keeps this list honest as app.js
	// grows modules.
	mux.Handle("GET /capture.js", http.FileServerFS(s.static))
	mux.Handle("GET /stream.js", http.FileServerFS(s.static))
	mux.Handle("GET /voxl/", s.voxl)
	mux.Handle("GET /brand/", s.brand)
	mux.HandleFunc("GET /shots/{name}", s.serveShot)
	mux.HandleFunc("GET /api/stream", s.serveStream)
	mux.HandleFunc("GET /api/session", s.serveSession)
	mux.HandleFunc("POST /api/display", s.serveResize)
	mux.HandleFunc("POST /api/scale", s.serveScale)
	mux.HandleFunc("GET /api/feedback", s.serveFeedbackList)
	mux.HandleFunc("POST /api/feedback", s.serveFeedbackCreate)
	mux.HandleFunc("PATCH /api/feedback/{id}", s.serveFeedbackUpdate)
	mux.HandleFunc("DELETE /api/feedback/{id}", s.serveFeedbackDelete)
	return s.access.guard(mux)
}

// revalidated has the browser check before reusing what it cached. voxl's
// client and worker speak the stream's protocol, and a new desktop feature
// brings a new voxl: a client cached from the old one would be talking to the
// new stream.
func revalidated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP lets the server be mounted directly, which the tests do.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Serve runs until ctx is done. listener may be nil, in which case Config.Addr
// is bound; under systemd the socket is passed in instead.
func Serve(ctx context.Context, log *slog.Logger, cfg Config, listener net.Listener) error {
	cfg.applyDefaults()
	server, err := New(log, cfg)
	if err != nil {
		return err
	}
	if listener == nil {
		var config net.ListenConfig
		listener, err = config.Listen(ctx, "tcp", cfg.Addr)
		if err != nil {
			return fmt.Errorf("desktop: listen on %s: %w", cfg.Addr, err)
		}
	}
	// Settling the scale is the last of startup, and it reads a file rather
	// than a screen. Nothing on this path may touch X — see settleScale.
	go server.settleScale()
	defer server.streamer.Close()

	httpServer := &http.Server{
		Handler: server,
		// The stream's websocket is idle whenever the desktop is, so neither the
		// read nor the write side may time out; the header deadline still bounds
		// a connection that never says what it wants.
		ReadHeaderTimeout: 15 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	log.Info("desktop viewer serving", "addr", listener.Addr(), "feedback", server.store.MarkdownPath())
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// settleScale writes the scale the desktop session will start at, and then lets
// it start. It is the reason this unit is Type=notify.
//
// **It must not touch X.** Anything that connects to /tmp/.X11-unix/X0 — an
// `xrandr` readiness check, say — socket-activates the X server, and
// xvfb.service pulls up the Xfce session behind it. Merely *starting the
// viewer* would then start the entire desktop: an X server, a window manager,
// a panel and the stream, from a process whose only job at that moment is to
// be ready to serve a web page. Anything that opened a TCP connection to 6900
// and went away — a health check, a port scan, a stray probe — would bring the
// whole thing up.
//
// The scale it needs is a file, not a screen: scale.env records what the last
// run settled on, and reading it costs nothing and starts nothing. What the
// session actually reads is that same file, so writing it is the whole job.
//
// X starts only when something genuinely needs pixels — a browser loading the
// page and asking for a size, or opening the stream, or any program in the
// sandbox talking to DISPLAY=:0. Not before.
func (s *Server) settleScale() {
	// Nothing remembered is rare in the image, where the boot flow has already
	// written the starting scale; it is the case of a viewer run anywhere else.
	// Either way the session starts at the default, 2x, and stays there until a
	// person picks another: no viewer's screen moves it.
	scale, remembered := StartingScale(s.display.EnvDir)
	if err := s.display.AdoptScale(scale); err != nil {
		s.log.Warn("write the desktop scale", "scale", scale, "error", err)
	}
	reason := "no scale was remembered, so the session starts at the default"
	if remembered {
		reason = "the remembered scale was written out"
	}
	s.release(s.log, reason)
}

// release lets the desktop session start, and tells the display that anything
// further is a change to a desktop that is already running.
func (s *Server) release(log *slog.Logger, why string) {
	s.releaseOnce.Do(func() {
		s.display.Release()
		if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			log.Warn("notify systemd of readiness", "error", err)
		}
		log.Info("desktop session released", "reason", why)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, _ *http.Request) {
	data, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveStream hands the browser's stream to voxl, starting it if it is not
// running. Serving it from this origin rather than pointing the page at voxl's
// port means one port is the whole desktop: one thing to forward, and one thing
// to reach; and it is what keeps voxl's own listener and its token out of
// every browser's reach (see access.go).
//
// It is the request that needs pixels, so it is allowed to start X: the
// geometry is read first, which brings the display up if it was cold and
// answers only once it is, and voxl is started only after that, because a
// capture started against a display that is not there exits.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.fail(w, http.StatusBadRequest, errors.New("desktop: the stream is a websocket"))
		return
	}
	if _, err := s.display.Geometry(r.Context()); err != nil {
		s.fail(w, http.StatusServiceUnavailable, fmt.Errorf("desktop: the display is not up: %w", err))
		return
	}
	token, err := s.streamer.Ensure(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL = &url.URL{Scheme: "http", Host: s.streamer.Addr, Path: "/ws"}
			out.Out.Host = s.streamer.Addr
			// The browser's origin was checked here; voxl is told nothing
			// about it, and needs the token instead.
			out.Out.Header.Del("Origin")
			out.Out.Header.Del("Cookie")
			out.Out.Header.Set("Authorization", "Bearer "+token)
		},
		// Video, input and the clipboard: nothing may sit in a buffer waiting
		// for more, or the desktop moves in bursts.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.log.Warn("desktop stream proxy failed", "error", err)
			http.Error(w, "the desktop stream is not reachable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// serveShot serves a note's picture, and only ever as a picture.
//
// The shots directory is the sandbox user's, so anything in the box can put a
// file there, and this origin is one the person may have let read their
// clipboard (the clipboard panel's paste). A page served from here would be
// same-origin with the viewer: it could read that clipboard and post it back.
// So only a .png is served, as image/png whatever is in it, never sniffed into
// something else, and sandboxed so a browser opening it directly runs nothing.
func (s *Server) serveShot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !strings.EqualFold(path.Ext(name), ".png") {
		s.fail(w, http.StatusNotFound, fmt.Errorf("desktop: %q is not a note's picture", name))
		return
	}
	full, err := s.store.ShotPath(path.Join(shotsDir, name))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	http.ServeFile(w, r, full)
}

type sessionResponse struct {
	Display      string   `json:"display"`
	FeedbackPath string   `json:"feedbackPath"`
	FeedbackDir  string   `json:"feedbackDir"`
	Prompt       string   `json:"prompt"`
	Limits       limits   `json:"limits"`
	Geometry     Geometry `json:"geometry"`
}

// limits is what the display will do, for the page to report rather than to
// recompute. The page performs none of this arithmetic: it sends the frame it
// has in CSS pixels and sizes that frame from the framebuffer it gets back.
type limits struct {
	MinWidth  int `json:"minWidth"`
	MinHeight int `json:"minHeight"`
	MaxWidth  int `json:"maxWidth"`
	MaxHeight int `json:"maxHeight"`
	Bucket    int `json:"bucket"`
	MinScale  int `json:"minScale"`
	MaxScale  int `json:"maxScale"`
	BaseDPI   int `json:"baseDpi"`
}

func (s *Server) serveSession(w http.ResponseWriter, r *http.Request) {
	response := sessionResponse{
		Display:      s.display.Name,
		FeedbackPath: s.store.MarkdownPath(),
		FeedbackDir:  s.store.Dir(),
		Prompt:       s.prompt,
		Limits: limits{
			MinWidth: MinWidth, MinHeight: MinHeight,
			MaxWidth: MaxWidth, MaxHeight: MaxHeight,
			Bucket:   SizeBucket,
			MinScale: MinScale, MaxScale: MaxScale,
			BaseDPI: BaseDPI,
		},
	}
	// Only when X is already up. Reading the geometry means running xrandr,
	// and xrandr on a cold display is what starts one — so a client fetching
	// this would bring up the whole desktop before anybody had asked to see
	// it. The page needs the rest of the payload to render at all, and it
	// asks for a size of its own the moment it connects, which is the point
	// at which starting X is the right thing to do.
	if geometry, err := s.display.GeometryIfUp(r.Context()); err == nil {
		response.Geometry = geometry
	} else {
		s.log.Warn("read desktop geometry", "error", err)
	}
	s.writeJSON(w, http.StatusOK, response)
}

type resizeRequest struct {
	// The viewer's frame in CSS pixels. The framebuffer this becomes is the
	// display's arithmetic, not the page's.
	CSSWidth  int `json:"cssWidth"`
	CSSHeight int `json:"cssHeight"`
}

// serveResize is called on every settled window resize, and carries no density:
// the two are separate decisions, and a density that moved with the window
// would be latched differently by every program that launched during a drag.
func (s *Server) serveResize(w http.ResponseWriter, r *http.Request) {
	var request resizeRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	geometry, err := s.display.Resize(r.Context(), request.CSSWidth, request.CSSHeight)
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}
	s.writeJSON(w, http.StatusOK, geometry)
}

type scaleRequest struct {
	// Scale is a person's choice; it is rounded to a supported integer here
	// rather than in the page, so one place decides.
	Scale float64 `json:"scale"`
	// Auto marks the viewer's own report rather than a person's choice. Its
	// scale is ignored: the standing one is applied instead (Display.SetScale).
	Auto bool `json:"auto"`
}

// serveScale is called twice at most in a normal session: once by the viewer as
// it opens, to have the standing scale applied before it asks for a size, and
// again only if a person picks another. Never on a resize.
func (s *Server) serveScale(w http.ResponseWriter, r *http.Request) {
	var request scaleRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	before := s.display.Scale()
	geometry, restarted, err := s.display.SetScale(r.Context(), NormalizeScale(request.Scale), request.Auto)
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}
	if geometry.Scale != before {
		s.log.Info("desktop scale changed",
			"scale", geometry.Scale, "dpi", geometry.DPI, "auto", request.Auto,
			"restarted", restarted)
	}
	s.writeJSON(w, http.StatusOK, scaleResponse{Geometry: geometry, Restarted: restarted})
}

// scaleResponse is the geometry plus the one thing a person setting the density
// needs and the geometry cannot say: whether their desktop session was
// restarted under them. Re-selecting the density already in effect changes
// nothing and restarts nothing, so a viewer that always warned about losing
// windows would be lying most of the times it said it.
type scaleResponse struct {
	Geometry
	Restarted bool `json:"restarted"`
}

type feedbackResponse struct {
	Items  []Item `json:"items"`
	Path   string `json:"path"`
	Prompt string `json:"prompt"`
}

func (s *Server) serveFeedbackList(w http.ResponseWriter, _ *http.Request) {
	items, err := s.store.List()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []Item{}
	}
	s.writeJSON(w, http.StatusOK, feedbackResponse{Items: items, Path: s.store.MarkdownPath(), Prompt: s.prompt})
}

type createRequest struct {
	Comment string `json:"comment"`
	Region  Region `json:"region"`
	// Screenshot is a PNG data URL cropped to Region, or empty when the page
	// could not read the framebuffer.
	Screenshot string `json:"screenshot"`
	// Screen is the whole desktop at the moment of capture with the region
	// drawn on it, same encoding. Optional for the same reason.
	Screen string `json:"screen"`
}

func (s *Server) serveFeedbackCreate(w http.ResponseWriter, r *http.Request) {
	var request createRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	png, err := decodePNG(request.Screenshot)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	screen, err := decodePNG(request.Screen)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	item, err := s.store.Add(request.Comment, request.Region, png, screen)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("desktop feedback captured", "id", item.ID, "file", s.store.MarkdownPath())
	s.writeJSON(w, http.StatusCreated, item)
}

type updateRequest struct {
	// Comment replaces the note's words. Empty leaves them alone, which is what
	// a request that only ticks the box sends.
	Comment string `json:"comment,omitempty"`
	// Done ticks or unticks the note. Optional so an edit to the words does not
	// have to restate it.
	Done *bool `json:"done,omitempty"`
}

// serveFeedbackUpdate edits one note: its words, whether it is done, or both.
//
// Ticking is offered here and deliberately not to the agent. The agent's half
// is making the change; saying the change is right is the half that has to come
// from whoever asked for it — an agent that closes its own note has kept a
// checklist, not had a review.
func (s *Server) serveFeedbackUpdate(w http.ResponseWriter, r *http.Request) {
	var request updateRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	id := r.PathValue("id")
	var item Item
	if strings.TrimSpace(request.Comment) != "" {
		updated, err := s.store.Update(id, request.Comment)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		item = updated
	}
	if request.Done != nil {
		updated, err := s.store.SetDone(id, *request.Done)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		item = updated
	}
	if item.ID == "" {
		s.fail(w, http.StatusBadRequest, errors.New("desktop: nothing to change"))
		return
	}
	s.log.Info("desktop feedback edited", "id", id)
	s.serveFeedbackList(w, r)
}

func (s *Server) serveFeedbackDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("desktop feedback deleted", "id", r.PathValue("id"))
	s.serveFeedbackList(w, r)
}

// decodePNG accepts the data URL a canvas produces, or nothing.
func decodePNG(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(value, prefix) {
		return nil, errors.New("desktop: screenshot must be a base64 PNG data URL")
	}
	png, err := base64.StdEncoding.DecodeString(value[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("desktop: decode screenshot: %w", err)
	}
	if len(png) > maxScreenshot {
		return nil, errors.New("desktop: screenshot is too large")
	}
	return png, nil
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(into); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("desktop: parse request: %w", err))
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Warn("write desktop response", "error", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.log.Warn("desktop request failed", "status", status, "error", err)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); err != nil {
		s.log.Warn("write desktop error response", "error", err)
	}
}

// expandHome resolves a leading ~ against this process's own account. systemd
// sets HOME for a unit with User=, but this also runs from a shell and from
// tests, so the passwd entry is the fallback rather than the other way round.
func expandHome(dir string) (string, error) {
	if dir != "~" && !strings.HasPrefix(dir, "~/") {
		return dir, nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		current, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("desktop: resolve home directory: %w", err)
		}
		home = current.HomeDir
	}
	if home == "" {
		return "", errors.New("desktop: cannot resolve ~ with no home directory")
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/")), nil
}
