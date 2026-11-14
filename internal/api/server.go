package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/web"
)

// HandlerTimeout bounds every API handler (spec 11.1). Downloads and the
// event stream are exempt because they are long-lived by nature.
const HandlerTimeout = 30 * time.Second

// maxBody bounds request bodies (certificate uploads, CSV imports).
const maxBody = 4 << 20

// Server is the web server. It lives in the guardian process.
type Server struct {
	backend  Backend
	dataRoot string
	log      *slog.Logger
	users    *UserStore
	sessions *sessions
	hub      *hub
	handler  http.Handler
	static   http.Handler

	cert     atomic.Pointer[tls.Certificate]
	certInfo atomic.Pointer[CertInfo]

	mu      sync.Mutex
	srv     *http.Server
	cfg     config.Server
	https   atomic.Bool
	subnets atomic.Pointer[[]*net.IPNet]
	closed  bool
}

// NewServer prepares the web server: accounts, certificate and routes.
func NewServer(b Backend, dataRoot string, log *slog.Logger) (*Server, error) {
	users, err := OpenUserStore(dataRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{backend: b, dataRoot: dataRoot, log: log.With("component", "api"), users: users,
		sessions: newSessions(), hub: newHub()}
	if users.Count() == 0 {
		s.log.Warn("no user accounts exist yet; create the first administrator with: buttercams setup init --admin-user NAME --admin-password PASSWORD")
	}
	if err := EnsureCertificate(dataRoot); err != nil {
		return nil, fmt.Errorf("creating the certificate: %w", err)
	}
	if err := s.reloadCertificate(); err != nil {
		return nil, err
	}
	s.static = s.staticHandler()
	s.handler = s.routes()
	return s, nil
}

// Users returns the account store.
func (s *Server) Users() *UserStore { return s.users }

func (s *Server) reloadCertificate() error {
	cert, info, err := LoadCertificate(s.dataRoot)
	if err != nil {
		return err
	}
	s.cert.Store(cert)
	s.certInfo.Store(&info)
	return nil
}

func parseSubnets(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range list {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Start begins listening with the given settings.
func (s *Server) Start(cfg config.Server) error {
	addr := net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	subnets := parseSubnets(cfg.AllowedSubnets)
	s.subnets.Store(&subnets)
	s.https.Store(cfg.HTTPS)

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	if cfg.HTTPS {
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return s.cert.Load(), nil
			},
		}
		ln = tls.NewListener(ln, srv.TLSConfig)
	}
	s.mu.Lock()
	s.srv, s.cfg = srv, cfg
	s.mu.Unlock()

	scheme := "http"
	if cfg.HTTPS {
		scheme = "https"
	}
	s.log.Info("web server listening", "address", scheme+"://"+addr)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("web server panicked", "panic", fmt.Sprint(r))
			}
		}()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("web server stopped", "error", err.Error())
		}
	}()
	return nil
}

// Restart moves the listener to new settings shortly after the current
// request has been answered. If the new address cannot be used, the previous
// one is restored.
func (s *Server) Restart(next config.Server) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("restarting the web server panicked", "panic", fmt.Sprint(r))
			}
		}()
		time.Sleep(time.Second)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		old, prev := s.srv, s.cfg
		s.mu.Unlock()
		if old != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			old.Shutdown(ctx)
			cancel()
			old.Close()
		}
		if err := s.Start(next); err != nil {
			s.log.Error("the web server could not listen on the new address; keeping the previous one",
				"error", err.Error())
			s.backend.RecordEvent(index.Event{Source: "guardian", Type: "web_server_failed", Severity: "critical",
				Data: map[string]any{"message": "The settings page could not move to the new address (" + err.Error() +
					"). It is still reachable at the previous address."}})
			if err := s.Start(prev); err != nil {
				s.log.Error("the web server could not be restored", "error", err.Error())
			}
		}
	}()
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	srv := s.srv
	s.mu.Unlock()
	s.hub.closeAll()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		srv.Shutdown(ctx)
		cancel()
		srv.Close()
	}
}

// ------------------------------------------------------------- middleware

type ctxKey int

const sessionKey ctxKey = 1

func sessionOf(r *http.Request) (session, bool) {
	s, ok := r.Context().Value(sessionKey).(session)
	return s, ok
}

type apiError struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  []config.FieldError `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": apiError{Code: code, Message: msg}})
}

// writeFail turns an error into the right response.
func (s *Server) writeFail(w http.ResponseWriter, err error) {
	var ve *config.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": apiError{
			Code: "validation_failed", Message: "Fix the highlighted fields.", Fields: ve.Fields}})
	case errors.Is(err, errNotFound), errors.Is(err, ErrNoSuchUser):
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, ErrLastAdmin):
		writeError(w, http.StatusConflict, "conflict", "The last administrator cannot be removed or demoted.")
	default:
		var ce *conflictError
		if errors.As(err, &ce) {
			writeError(w, http.StatusConflict, "conflict", ce.msg)
			return
		}
		s.log.Error("request failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on the server. The details are in the log.")
	}
}

var errNotFound = errors.New("not found")

type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }

func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func isLoopback(r *http.Request) bool {
	ip := clientIP(r)
	return ip != nil && ip.IsLoopback()
}

// wrap applies, outermost first: panic recovery, the subnet filter, security
// headers.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				// A failing handler never affects recording (spec 9).
				s.log.Error("request handler panicked", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				defer func() { recover() }()
				writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on the server. The details are in the log.")
			}
		}()
		if nets := s.subnets.Load(); nets != nil && len(*nets) > 0 && !isLoopback(r) {
			ip := clientIP(r)
			allowed := false
			for _, n := range *nets {
				if ip != nil && n.Contains(ip) {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "Access from this address is not allowed.", http.StatusForbidden)
				return
			}
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		if s.https.Load() {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type access int

const (
	accessPublic access = iota // no session needed
	accessViewer               // any signed-in user
	accessAdmin                // administrators only
)

// guard checks the session, the role and the CSRF token.
func (s *Server) guard(level access, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if level == accessPublic {
			next(w, r)
			return
		}
		var sess session
		ok := false
		if c, err := r.Cookie(SessionCookie); err == nil {
			sess, ok = s.sessions.get(c.Value)
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
			return
		}
		if level == accessAdmin && sess.role != RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "This needs an administrator account.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if sess.role != RoleAdmin && r.URL.Path != "/api/v1/auth/logout" {
				writeError(w, http.StatusForbidden, "forbidden", "This needs an administrator account.")
				return
			}
			token := r.Header.Get(CSRFHeader)
			if len(token) != len(sess.csrf) || !constantTimeEqual(token, sess.csrf) {
				writeError(w, http.StatusForbidden, "csrf_failed", "The request was refused for safety. Reload the page and try again.")
				return
			}
		}
		// Keep the cookie alive while the session is in use.
		if time.Since(sess.issued) > time.Hour {
			s.setCookie(w, sess.token)
			s.sessions.reissued(sess.token)
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, sess)))
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func (s *Server) setCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.https.Load(),
		SameSite: http.SameSiteStrictMode, MaxAge: int(SessionIdleTimeout / time.Second),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.https.Load(),
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// timed gives a handler the standard 30 s limit.
func timed(h http.HandlerFunc) http.Handler {
	body, _ := json.Marshal(map[string]any{"error": apiError{Code: "timeout", Message: "The server took too long to answer. Try again."}})
	th := http.TimeoutHandler(h, HandlerTimeout, string(body))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		th.ServeHTTP(w, r)
	})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return FieldErr("", "The request could not be read: "+err.Error())
	}
	return nil
}

// ----------------------------------------------------------------- routes

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	api := func(pattern string, level access, h http.HandlerFunc) {
		mux.Handle(pattern, timed(s.guard(level, h)))
	}
	const p = "/api/v1"

	api("POST "+p+"/auth/login", accessPublic, s.handleLogin)
	api("POST "+p+"/auth/logout", accessViewer, s.handleLogout)
	api("GET "+p+"/auth/session", accessViewer, s.handleSession)

	api("GET "+p+"/health", accessPublic, s.handleHealth)

	api("GET "+p+"/cameras", accessViewer, s.handleCamerasList)
	api("POST "+p+"/cameras", accessAdmin, s.handleCameraCreate)
	api("POST "+p+"/cameras/test", accessAdmin, s.handleCameraTest)
	api("POST "+p+"/cameras/import", accessAdmin, s.handleCameraImport)
	api("PUT "+p+"/cameras/{id}", accessAdmin, s.handleCameraUpdate)
	api("DELETE "+p+"/cameras/{id}", accessAdmin, s.handleCameraDelete)

	api("GET "+p+"/destinations", accessViewer, s.handleDestinationsList)
	api("POST "+p+"/destinations", accessAdmin, s.handleDestinationCreate)
	api("POST "+p+"/destinations/test", accessAdmin, s.handleDestinationTest)
	api("PUT "+p+"/destinations/{id}", accessAdmin, s.handleDestinationUpdate)
	api("DELETE "+p+"/destinations/{id}", accessAdmin, s.handleDestinationDelete)

	api("GET "+p+"/recordings/cameras", accessViewer, s.handleRecordingCameras)
	api("GET "+p+"/segments", accessViewer, s.handleSegments)
	mux.Handle("GET "+p+"/segments/{id}/download", s.guard(accessViewer, s.handleDownload)) // long-lived

	api("GET "+p+"/events", accessViewer, s.handleEvents)

	api("GET "+p+"/system", accessViewer, s.handleSystemGet)
	api("PUT "+p+"/system", accessAdmin, s.handleSystemPut)
	api("POST "+p+"/system/rebuild-index", accessAdmin, s.handleRebuild)
	api("POST "+p+"/system/certificate", accessAdmin, s.handleCertificate)
	api("GET "+p+"/system/config/export", accessAdmin, s.handleConfigExport)
	api("POST "+p+"/system/config/import", accessAdmin, s.handleConfigImport)

	api("GET "+p+"/users", accessAdmin, s.handleUsersList)
	api("POST "+p+"/users", accessAdmin, s.handleUserCreate)
	api("PUT "+p+"/users/{username}", accessAdmin, s.handleUserUpdate)
	api("DELETE "+p+"/users/{username}", accessAdmin, s.handleUserDelete)

	mux.Handle("GET "+p+"/stream", s.guard(accessViewer, s.handleStream)) // long-lived

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No such API endpoint.")
	})
	mux.Handle("/", s.static)
	return s.wrap(mux)
}

// staticHandler serves the embedded UI. Any path that is not a file falls
// back to index.html so the single-page app can handle its own routes.
func (s *Server) staticHandler() http.Handler {
	if !web.Built() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><title>ButterCams</title><h1>ButterCams</h1>
<p>This build does not include the settings page. Build it with <code>npm install &amp;&amp; npm run build</code>
in the <code>web</code> folder and compile ButterCams again. The API is available under <code>/api/v1</code>.</p>`)
		})
	}
	dist := web.Dist()
	files := http.FileServerFS(dist)
	index, _ := fs.ReadFile(dist, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name != "" {
			if st, err := fs.Stat(dist, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
