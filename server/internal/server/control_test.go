package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.RemoteAddr = "192.0.2.1:123" },
		func(r *http.Request) { r.Host = "attacker.example" },
		func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
		func(r *http.Request) { r.Header.Set("X-Forwarded-For", "192.0.2.1") },
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/setup", strings.NewReader(`{}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Romsync-Request", "1")
		mutate(r)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("nonlocal setup allowed: %d", w.Code)
		}
	}
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
	settings.DashboardURL = "https://romsync.example:8443"
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"romsync.example:8443", "attacker.example", "romsync.example:9443"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/settings", nil)
		r.Host = host
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		expected := http.StatusForbidden
		if host == "romsync.example:8443" {
			expected = http.StatusOK
		}
		if w.Code != expected {
			t.Fatalf("host %s returned %d want %d", host, w.Code, expected)
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
}

func TestLoginRateLimitAndSecureCookie(t *testing.T) {
	c, store, settings := newTestControl(t)
	settings.DashboardURL = "https://romsync.example"
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
