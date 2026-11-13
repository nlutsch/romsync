package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controlRequest(c *Control, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Romsync-Request", "1")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	return w
}

func newTestControl(t *testing.T) (*Control, *ConfigStore, Settings) {
	t.Helper()
	store, settings := testConfig(t)
	c, err := NewControl(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetRuntime(nil, settings, nil)
	return c, store, settings
}

func TestHostDirectoryBrowsing(t *testing.T) {
	c, store, settings := newTestControl(t)
	r := httptest.NewRequest("GET", "/api/directories", nil)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	decodeResponse[map[string]string](t, w, 403)
	base := filepath.Dir(store.Path())
	if err := os.WriteFile(filepath.Join(base, "not-a-directory"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	type listing struct {
		Path        string `json:"path"`
		Parent      string `json:"parent"`
		Directories []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"directories"`
	}
	result := decodeResponse[listing](t, controlRequest(c, "GET", "/api/directories", "", nil), 200)
	if result.Path != base || result.Parent != filepath.Dir(base) || len(result.Directories) != 1 ||
		result.Directories[0].Name != "roms" || result.Directories[0].Path != settings.ROMPath {
		t.Fatalf("incorrect host directory listing: %+v", result)
	}
	empty := decodeResponse[listing](t, controlRequest(c, "GET", "/api/directories?path=roms", "", nil), 200)
	if empty.Path != settings.ROMPath || empty.Directories == nil || len(empty.Directories) != 0 {
		t.Fatalf("incorrect empty directory listing: %+v", empty)
	}
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/directories?path=missing", "", nil), 404)
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/directories?path=not-a-directory", "", nil), 400)

	root := filepath.VolumeName(base) + string(os.PathSeparator)
	atRoot := decodeResponse[listing](t, controlRequest(c, "GET", "/api/directories?path="+url.QueryEscape(root), "", nil), 200)
	if atRoot.Parent != "" {
		t.Fatal("filesystem root must not have a parent")
	}
	auth, err := MakeLogin("admin", "long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(auth, true, &settings); err != nil {
		t.Fatal(err)
	}
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/directories", "", nil), 401)
	login := controlRequest(c, "POST", "/api/login", `{"username":"admin","password":"long-test-password"}`, nil)
	decodeResponse[map[string]bool](t, login, 200)
	decodeResponse[listing](t, controlRequest(c, "GET", "/api/directories", "", login.Result().Cookies()[0]), 200)
}

func TestFirstRunLoginAndSessions(t *testing.T) {
	c, store, settings := newTestControl(t)
	status := decodeResponse[map[string]any](t, controlRequest(c, "GET", "/api/status", "", nil), 200)
	if status["configured"] != false || status["defaults"] == nil {
		t.Fatal("setup defaults missing")
	}
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/settings", "", nil), 401)
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatal("config written before setup")
	}
	body := jsonBody(t, map[string]any{"settings": settings, "mode": "login", "username": "admin", "password": "long-test-password"})
	decodeResponse[map[string]any](t, controlRequest(c, "POST", "/api/setup", body, nil), 201)
	select {
	case <-c.Restarts():
	default:
		t.Fatal("setup did not start services")
	}
	c.SetRuntime(nil, settings, nil)
	decodeResponse[map[string]string](t, controlRequest(c, "POST", "/api/setup", body, nil), 409)
	decodeResponse[map[string]string](t, controlRequest(c, "POST", "/api/login", `{"username":"bad","password":"wrong"}`, nil), 401)
	response := controlRequest(c, "POST", "/api/login", `{"username":"admin","password":"long-test-password"}`, nil)
	decodeResponse[map[string]bool](t, response, 200)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 43200 {
		t.Fatalf("unsafe session cookie: %v", cookies)
	}
	cookie := cookies[0]
	view := controlRequest(c, "GET", "/api/settings", "", cookie)
	decodeResponse[map[string]any](t, view, 200)
	if strings.Contains(view.Body.String(), "password_hash") || strings.Contains(view.Body.String(), store.Snapshot().Auth.Salt) {
		t.Fatal("password material exposed")
	}
	c.SetRuntime(nil, settings, nil)
	decodeResponse[map[string]any](t, controlRequest(c, "GET", "/api/settings", "", cookie), 200)
	decodeResponse[map[string]bool](t, controlRequest(c, "POST", "/api/logout", "{}", cookie), 200)
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/settings", "", cookie), 401)
}

func TestAnonymousSettingsAndAuthChanges(t *testing.T) {
	c, store, settings := newTestControl(t)
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	settings.MaxROMBytes = 1000
	decodeResponse[map[string]bool](t, controlRequest(c, "PUT", "/api/settings", jsonBody(t, settings), nil), 200)
	if store.Snapshot().Settings.MaxROMBytes != 1000 {
		t.Fatal("settings not saved")
	}
	decodeResponse[map[string]bool](t, controlRequest(c, "POST", "/api/auth", `{"mode":"login","username":"owner","password":"new-long-password"}`, nil), 200)
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/settings", "", nil), 401)
	login := controlRequest(c, "POST", "/api/login", `{"username":"owner","password":"new-long-password"}`, nil)
	decodeResponse[map[string]bool](t, login, 200)
	cookie := login.Result().Cookies()[0]
	decodeResponse[map[string]string](t, controlRequest(c, "POST", "/api/auth", `{"mode":"anonymous","current_password":"wrong"}`, cookie), 401)
	decodeResponse[map[string]bool](t, controlRequest(c, "POST", "/api/auth", `{"mode":"anonymous","current_password":"new-long-password"}`, cookie), 200)
	if len(c.sessions) != 0 || store.Snapshot().Auth != (DashboardAuth{Mode: "anonymous"}) {
		t.Fatal("old credentials/sessions retained")
	}
	decodeResponse[map[string]any](t, controlRequest(c, "GET", "/api/settings", "", nil), 200)
	reloaded, err := LoadConfig(store.Path())
	if err != nil || reloaded.Snapshot().Auth.Mode != "anonymous" {
		t.Fatal("anonymous mode not persistent", err)
	}
}

func TestDashboardRequestBoundaries(t *testing.T) {
	c, store, settings := newTestControl(t)
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		func(r *http.Request) { r.Header.Del("X-Romsync-Request") },
	} {
		r := httptest.NewRequest("PUT", "http://127.0.0.1:8080/api/settings", strings.NewReader(jsonBody(t, settings)))
		r.Header.Set("X-Romsync-Request", "1")
		mutate(r)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("cross-origin write allowed: %d", w.Code)
		}
	}
	for _, body := range []string{`{"bad":1}`, `{} {}`, strings.Repeat("x", 17000)} {
		decodeResponse[map[string]string](t, controlRequest(c, "PUT", "/api/settings", body, nil), 400)
	}
}

func TestRemoteFirstRunSetup(t *testing.T) {
	c, _, settings := newTestControl(t)
	r := httptest.NewRequest("GET", "http://server.example:18777/api/status", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	status := decodeResponse[map[string]any](t, w, 200)
	if status["defaults"] == nil {
		t.Fatal("remote setup defaults missing")
	}
	body := jsonBody(t, map[string]any{"settings": settings, "mode": "anonymous"})
	r = httptest.NewRequest("POST", "https://dashboard.example/api/setup", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Romsync-Request", "1")
	r.Header.Set("Origin", "https://dashboard.example")
	w = httptest.NewRecorder()
	c.ServeHTTP(w, r)
	decodeResponse[map[string]any](t, w, 201)
}

func TestSessionsExpireAndProcessRestartInvalidates(t *testing.T) {
	c, store, settings := newTestControl(t)
	auth, err := MakeLogin("admin", "long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(auth, true, &settings); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: "expired-test-session"}
	c.sessions[digest(cookie.Value)] = dashboardSession{expires: time.Now().Add(-time.Second), auth: auth}
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/settings", "", cookie), 401)
	c.sessions[digest(cookie.Value)] = dashboardSession{expires: time.Now().Add(time.Hour), auth: auth}
	fresh, err := NewControl(store, c.log)
	if err != nil {
		t.Fatal(err)
	}
	decodeResponse[map[string]string](t, controlRequest(fresh, "GET", "/api/settings", "", cookie), 401)
	replacement, err := MakeLogin("admin", "replacement-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(replacement, false, nil); err != nil {
		t.Fatal(err)
	}
	decodeResponse[map[string]string](t, controlRequest(c, "GET", "/api/settings", "", cookie), 401)
}

func TestPublicDashboardHostAndProxyOrigin(t *testing.T) {
	c, store, settings := newTestControl(t)
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"192.0.2.1:18777", "server.example:18777", "another.example", "romsync.example:8443", "romsync.example:9443"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/settings", nil)
		r.Host = host
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("host %s returned %d", host, w.Code)
		}
	}
	r := httptest.NewRequest("PUT", "http://127.0.0.1/api/settings", strings.NewReader(jsonBody(t, settings)))
	r.Host = "romsync.example:8443"
	r.Header.Set("X-Romsync-Request", "1")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Origin", "https://romsync.example:8443")
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	decodeResponse[map[string]bool](t, w, 200)

	r = httptest.NewRequest("PUT", "http://192.0.2.1:18777/api/settings", strings.NewReader(jsonBody(t, settings)))
	r.Host = "server.example:18777"
	r.Header.Set("X-Romsync-Request", "1")
	r.Header.Set("Origin", "http://server.example:18777")
	w = httptest.NewRecorder()
	c.ServeHTTP(w, r)
	decodeResponse[map[string]bool](t, w, 200)
}

func TestLoginRateLimitAndSecureCookie(t *testing.T) {
	c, store, settings := newTestControl(t)
	auth, err := MakeLogin("admin", "long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(auth, true, &settings); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://romsync.example/api/login", strings.NewReader(`{"username":"admin","password":"long-test-password"}`))
	r.Header.Set("X-Romsync-Request", "1")
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	decodeResponse[map[string]bool](t, w, 200)
	if !w.Result().Cookies()[0].Secure {
		t.Fatal("HTTPS cookie is not secure")
	}
	c.window, c.attempts = time.Now(), 20
	limited := controlRequest(c, "POST", "/api/login", `{"username":"admin","password":"long-test-password"}`, nil)
	decodeResponse[map[string]string](t, limited, 429)
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry guidance")
	}
}
