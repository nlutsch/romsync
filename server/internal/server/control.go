package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "romsync_session"

type dashboardSession struct {
	expires time.Time
	auth    DashboardAuth
}

// Control stays available without a ROM directory, including after a failed service restart.
type Control struct {
	store      *ConfigStore
	log        *slog.Logger
	static     http.Handler
	restart    chan struct{}
	mu         sync.RWMutex
	active     *Admin
	applied    Settings
	lastErr    string
	busy       bool
	generation uint64
	authMu     sync.Mutex
	sessions   map[string]dashboardSession
	attempts   int
	window     time.Time
	loginBusy  bool
}

func NewControl(store *ConfigStore, log *slog.Logger) (*Control, error) {
	files, err := fs.Sub(adminAssets, "web")
	if err != nil {
		return nil, err
	}
	return &Control{store: store, log: log, static: http.FileServer(http.FS(files)),
		restart: make(chan struct{}, 1), sessions: map[string]dashboardSession{}}, nil
}

func (c *Control) Restarts() <-chan struct{} { return c.restart }

func (c *Control) SetRuntime(active *Admin, settings Settings, runtimeErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active, c.applied, c.busy, c.lastErr = active, settings, false, ""
	c.generation++
	if runtimeErr != nil {
		c.lastErr = runtimeErr.Error()
		c.log.Error("sync services unavailable; correct settings in dashboard", "error", runtimeErr)
	}
}

func (c *Control) Detach() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = nil
	c.busy = true
}

func (c *Control) reserveRestart() (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy {
		return c.generation, false
	}
	c.busy = true
	return c.generation + 1, true
}

func (c *Control) requestRestart() {
	select {
	case c.restart <- struct{}{}:
	default:
	}
}

func (c *Control) fail(w http.ResponseWriter, code int, message string) {
	c.json(w, code, map[string]string{"error": message})
}

func (c *Control) json(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		c.log.Error("dashboard response failed", "error", err)
	}
}

func (c *Control) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		c.fail(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		c.fail(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func (c *Control) authorized(r *http.Request, auth DashboardAuth) bool {
	if auth.Mode == "anonymous" {
		return true
	}
	if auth.Mode != "login" {
		return false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	s, ok := c.sessions[digest(cookie.Value)]
	return ok && s.auth == auth && time.Now().Before(s.expires)
}

func (c *Control) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		c.static.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		c.fail(w, http.StatusForbidden, "cross-site requests are not allowed")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// A custom header blocks cross-origin forms and simple requests, including in anonymous mode.
		if r.Header.Get("X-Romsync-Request") != "1" {
			c.fail(w, http.StatusForbidden, "X-Romsync-Request: 1 header required")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			if err != nil || !strings.EqualFold(u.Host, r.Host) || u.Scheme != scheme {
				c.fail(w, http.StatusForbidden, "request origin does not match dashboard")
				return
			}
		}
	}
	auth := c.store.Snapshot().Auth
	if r.Method == http.MethodGet && r.URL.Path == "/api/directories" {
		if auth.Mode != "" && !c.authorized(r, auth) {
			c.fail(w, http.StatusUnauthorized, "dashboard login required")
			return
		}
		if r.Header.Get("X-Romsync-Request") != "1" {
			c.fail(w, http.StatusForbidden, "X-Romsync-Request: 1 header required")
			return
		}
		c.directories(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/status":
		c.mu.RLock()
		ready, busy, lastErr, generation := c.active != nil, c.busy, c.lastErr, c.generation
		c.mu.RUnlock()
		status := map[string]any{"configured": auth.Mode != "", "mode": auth.Mode,
			"authenticated": c.authorized(r, auth), "ready": ready, "restarting": busy,
			"generation": generation}
		if c.authorized(r, auth) {
			status["runtime_error"] = lastErr
		}
		if auth.Mode == "" {
			status["defaults"] = c.store.Snapshot().Settings
		}
		c.json(w, http.StatusOK, status)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/setup":
		c.setup(w, r)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/api/login":
		c.login(w, r, auth)
		return
	}
	if !c.authorized(r, auth) {
		c.fail(w, http.StatusUnauthorized, "dashboard login required")
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/logout":
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			c.authMu.Lock()
			delete(c.sessions, digest(cookie.Value))
			c.authMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		c.json(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/api/settings":
		config := c.store.Snapshot()
		c.mu.RLock()
		applied, lastErr, ready := c.applied, c.lastErr, c.active != nil
		c.mu.RUnlock()
		c.json(w, http.StatusOK, map[string]any{"settings": config.Settings, "config_path": c.store.Path(),
			"auth":             map[string]string{"mode": config.Auth.Mode, "username": config.Auth.Username},
			"restart_required": config.Settings != applied || !ready, "runtime_error": lastErr})
	case r.Method == http.MethodPut && r.URL.Path == "/api/settings":
		var settings Settings
		if !c.decode(w, r, &settings) {
			return
		}
		if err := c.store.SaveSettings(settings); err != nil {
			c.log.Error("configuration save failed", "error", err)
			c.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		c.json(w, http.StatusOK, map[string]bool{"saved": true, "restart_required": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/auth":
		if !c.beginAuth(w) {
			return
		}
		defer c.endAuth()
		var req struct {
			Mode            string `json:"mode"`
			Username        string `json:"username"`
			Password        string `json:"password"`
			CurrentPassword string `json:"current_password"`
		}
		if !c.decode(w, r, &req) {
			return
		}
		if auth.Mode == "login" && !verifyLogin(auth, auth.Username, req.CurrentPassword) {
			c.fail(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
		next, err := requestedAuth(req.Mode, req.Username, req.Password)
		if err == nil {
			err = c.store.SaveAuth(next, false, nil)
		}
		if err != nil {
			c.log.Error("authentication update failed", "error", err)
			c.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		c.authMu.Lock()
		clear(c.sessions)
		c.authMu.Unlock()
		c.json(w, http.StatusOK, map[string]bool{"saved": true})
	case r.Method == http.MethodPost && r.URL.Path == "/api/restart":
		generation, ok := c.reserveRestart()
		if !ok {
			c.fail(w, http.StatusConflict, "services are already restarting")
			return
		}
		c.json(w, http.StatusAccepted, map[string]any{"restarting": true, "generation": generation, "web_listen": c.store.Snapshot().Settings.WebListen})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		c.requestRestart()
	default:
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.active == nil || c.busy {
			c.fail(w, http.StatusServiceUnavailable, "sync services unavailable; check Settings")
			return
		}
		c.active.handler.ServeHTTP(w, r)
	}
}

func (c *Control) directories(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(c.store.Path()), path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err == nil && !info.IsDir() {
		c.fail(w, http.StatusBadRequest, "path must be a directory")
		return
	}
	var entries []os.DirEntry
	if err == nil {
		entries, err = os.ReadDir(path)
	}
	if err != nil {
		c.log.Error("directory browsing failed", "path", path, "error", err)
		code := http.StatusInternalServerError
		if errors.Is(err, fs.ErrNotExist) {
			code = http.StatusNotFound
		} else if errors.Is(err, fs.ErrPermission) {
			code = http.StatusForbidden
		}
		c.fail(w, code, err.Error())
		return
	}
	type directory struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	dirs := make([]directory, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, directory{Name: entry.Name(), Path: filepath.Join(path, entry.Name())})
		}
	}
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	c.json(w, http.StatusOK, struct {
		Path        string      `json:"path"`
		Parent      string      `json:"parent"`
		Directories []directory `json:"directories"`
	}{path, parent, dirs})
}

func requestedAuth(mode, username, password string) (DashboardAuth, error) {
	if mode == "anonymous" {
		return DashboardAuth{Mode: mode}, nil
	}
	if mode != "login" {
		return DashboardAuth{}, errors.New("choose login or anonymous access")
	}
	return MakeLogin(username, password)
}

func (c *Control) setup(w http.ResponseWriter, r *http.Request) {
	if c.store.Snapshot().Auth.Mode != "" {
		c.fail(w, http.StatusConflict, "setup has already been completed")
		return
	}
	if !c.beginAuth(w) {
		return
	}
	defer c.endAuth()
	var req struct {
		Settings Settings `json:"settings"`
		Mode     string   `json:"mode"`
		Username string   `json:"username"`
		Password string   `json:"password"`
	}
	if !c.decode(w, r, &req) {
		return
	}
	next, err := requestedAuth(req.Mode, req.Username, req.Password)
	if err == nil {
		err = c.store.SaveAuth(next, true, &req.Settings)
	}
	if err != nil {
		c.log.Error("initial setup failed", "error", err)
		c.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	generation, _ := c.reserveRestart()
	c.json(w, http.StatusCreated, map[string]any{"saved": true, "generation": generation, "web_listen": req.Settings.WebListen})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	c.requestRestart()
}

func (c *Control) login(w http.ResponseWriter, r *http.Request, auth DashboardAuth) {
	if auth.Mode != "login" {
		c.fail(w, http.StatusBadRequest, "password login is not enabled")
		return
	}
	var req struct{ Username, Password string }
	if !c.decode(w, r, &req) {
		return
	}
	if !c.beginAuth(w) {
		return
	}
	defer c.endAuth()
	if !verifyLogin(auth, req.Username, req.Password) {
		c.log.Warn("dashboard login failed")
		c.fail(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	id, err := randomID()
	if err != nil {
		c.log.Error("session creation failed", "error", err)
		c.fail(w, http.StatusInternalServerError, "could not create login session")
		return
	}
	c.authMu.Lock()
	now := time.Now()
	for key, session := range c.sessions {
		if !now.Before(session.expires) {
			delete(c.sessions, key)
		}
	}
	if len(c.sessions) >= 128 {
		c.authMu.Unlock()
		c.fail(w, http.StatusTooManyRequests, "session limit reached; log out another session or wait for expiry")
		return
	}
	c.sessions[digest(id)] = dashboardSession{expires: now.Add(12 * time.Hour), auth: auth}
	c.authMu.Unlock()
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: int((12 * time.Hour) / time.Second)})
	c.json(w, http.StatusOK, map[string]bool{"ok": true})
}

func (c *Control) beginAuth(w http.ResponseWriter) bool {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if time.Since(c.window) >= time.Minute {
		c.window, c.attempts = time.Now(), 0
	}
	if c.loginBusy || c.attempts >= 20 {
		w.Header().Set("Retry-After", "60")
		c.fail(w, http.StatusTooManyRequests, "too many login attempts; try again in one minute")
		return false
	}
	c.attempts++
	c.loginBusy = true
	return true
}

func (c *Control) endAuth() {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	c.loginBusy = false
}
