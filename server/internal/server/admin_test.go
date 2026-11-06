package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
)

const testDashboardSession = "test-only-session-not-for-production"

func setupAdmin(t *testing.T, f *fixture, maxROM int64) *Admin {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	store, err := LoadConfig(filepath.Join(t.TempDir(), "romsync.json"))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := MakeLogin("admin", "test-password-not-real")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(auth, false, nil); err != nil {
		t.Fatal(err)
	}
	control, err := NewControl(store, f.s.log)
	if err != nil {
		t.Fatal(err)
	}
	control.sessions[digest(testDashboardSession)] = dashboardSession{expires: time.Now().Add(time.Hour), auth: auth}
	admin, err := NewAdmin(f.ctx, f.s, AdminOptions{Control: control, MaxROMBytes: maxROM, Skyscraper: binary, Source: "screenscraper", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	control.SetRuntime(admin, store.Snapshot().Settings, nil)
	return admin
}

func adminRequest(t *testing.T, a *Admin, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+target, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testDashboardSession})
	r.Header.Set("X-Romsync-Request", "1")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func decodeResponse[T any](t *testing.T, w *httptest.ResponseRecorder, code int) T {
	t.Helper()
	if w.Code != code {
		t.Fatalf("got %d want %d: %s", w.Code, code, w.Body.String())
	}
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func waitJobs(t *testing.T, a *Admin) []MetadataJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		jobs := a.jobs.snapshot()
		if len(jobs) > 0 {
			active := false
			for _, job := range jobs {
				active = active || job.Status == "running" || job.Status == "queued"
			}
			if !active {
				return jobs
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("metadata job did not finish")
	return nil
}

func TestAdminAuthAndAssets(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	for _, endpoint := range []string{"/api/library", "/api/devices", "/api/jobs", "/api/media?id=x", "/api/roms"} {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080"+endpoint, nil)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s exposed: %d", endpoint, w.Code)
		}
		r.Header.Set("Authorization", "Bearer wrong-token")
		w = httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal("wrong token accepted")
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/library", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testDashboardSession})
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("cross-site request accepted")
	}
	for _, asset := range []string{"/", "/app.js", "/style.css"} {
		w := adminRequest(t, a, http.MethodGet, asset, "")
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("asset %s: %d", asset, w.Code)
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing response protections")
		}
	}
}

func TestAdminLibraryAndApproval(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	enrollment, err := f.client.RegisterDevice(f.ctx, &pb.RegisterDeviceRequest{Name: "<script>alert(1)</script>", Profile: pb.Profile_MIYOO})
	if err != nil {
		t.Fatal(err)
	}
	w := adminRequest(t, a, http.MethodGet, "/api/devices", "")
	list := decodeResponse[[]map[string]any](t, w, http.StatusOK)
	if len(list) != 1 || list[0]["approved"] != false || list[0]["profile"] != "MIYOO" {
		t.Fatalf("devices: %v", list)
	}
	if strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), enrollment.Token) {
		t.Fatal("credentials exposed")
	}
	for _, approved := range []bool{true, false} {
		body := `{"approved":false}`
		if approved {
			body = `{"approved":true}`
		}
		decodeResponse[map[string]bool](t, adminRequest(t, a, http.MethodPost, "/api/devices/"+enrollment.DeviceId, body), http.StatusOK)
		var d Device
		if err := readJSON(f.s.state, filepath.Join("devices", enrollment.DeviceId+".json"), &d); err != nil {
			t.Fatal(err)
		}
		if d.Approved != approved {
			t.Fatal("approval not persisted")
		}
	}
	var response struct {
		Games     []adminGame `json:"games"`
		Platforms []string    `json:"platforms"`
	}
	response = decodeResponse[struct {
		Games     []adminGame `json:"games"`
		Platforms []string    `json:"platforms"`
	}](t, adminRequest(t, a, http.MethodGet, "/api/library", ""), http.StatusOK)
	if len(response.Games) != 2 || len(response.Platforms) != 2 {
		t.Fatalf("library: %v", response)
	}
	w = adminRequest(t, a, http.MethodPost, "/api/devices/"+enrollment.DeviceId, `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatal("missing approval accepted")
	}
	w = adminRequest(t, a, http.MethodPost, "/api/devices/"+digest("missing"), `{"approved":true}`)
	if w.Code != http.StatusNotFound {
		t.Fatal("unknown device accepted")
	}
}

func TestAdminUploadAndAutomaticEnrichment(t *testing.T) {
	t.Setenv("ROMSYNC_TEST_SCRAPER", "1")
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	w := adminRequest(t, a, http.MethodPut, "/api/roms?platform=snes&name=game.sfc", "test ROM")
	uploaded := decodeResponse[struct {
		ID       string      `json:"id"`
		Job      MetadataJob `json:"job"`
		Warnings []string    `json:"warnings"`
	}](t, w, http.StatusCreated)
	if uploaded.ID != "snes/game.sfc" || uploaded.Job.Platform != "snes" || len(uploaded.Warnings) != 0 {
		t.Fatalf("upload: %+v", uploaded)
	}
	jobs := waitJobs(t, a)
	if jobs[0].Status != "succeeded" {
		t.Fatalf("enrichment failed: %+v", jobs[0])
	}
	if data, err := os.ReadFile(filepath.Join(f.roms, "snes", "game.sfc")); err != nil || string(data) != "test ROM" {
		t.Fatalf("upload missing: %s %v", data, err)
	}
	w = adminRequest(t, a, http.MethodGet, "/api/library", "")
	library := decodeResponse[struct {
		Games []adminGame `json:"games"`
	}](t, w, http.StatusOK)
	var found bool
	for _, g := range library.Games {
		if g.ID == "snes/game.sfc" {
			found = true
			if g.Title != "Test" || g.Image == "" {
				t.Fatalf("scraped data missing: %+v", g)
			}
			img := adminRequest(t, a, http.MethodGet, "/api/media?id="+url.QueryEscape(g.Image), "")
			if img.Code != http.StatusOK || img.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("image: %d", img.Code)
			}
		}
	}
	if !found {
		t.Fatal("uploaded ROM not cataloged")
	}
	w = adminRequest(t, a, http.MethodPut, "/api/roms?platform=snes&name=game.sfc", "replacement")
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate accepted: %d", w.Code)
	}
	data, err := os.ReadFile(filepath.Join(f.roms, "snes", "game.sfc"))
	if err != nil || string(data) != "test ROM" {
		t.Fatal("original ROM replaced")
	}
	decodeResponse[MetadataJob](t, adminRequest(t, a, http.MethodPost, "/api/jobs", `{"platform":"snes"}`), http.StatusAccepted)
	if jobs := waitJobs(t, a); jobs[0].Status != "succeeded" {
		t.Fatalf("manual enrichment: %+v", jobs)
	}
}

func TestAdminUploadValidation(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 8)
	for _, tc := range []struct {
		target, body string
		code         int
	}{
		{"/api/roms?platform=../outside&name=game.sfc", "ROM", 400},
		{"/api/roms?platform=SFC&name=game.sfc", "ROM", 400},
		{"/api/roms?platform=snes&name=..%2Fescape.sfc", "ROM", 400},
		{"/api/roms?platform=snes&name=bad%5Cescape.sfc", "ROM", 400},
		{"/api/roms?platform=snes&name=gamelist.xml", "ROM", 400},
		{"/api/roms?platform=snes&name=.hidden.sfc", "ROM", 400},
		{"/api/roms?platform=snes&name=empty.sfc", "", 400},
		{"/api/roms?platform=snes&name=large.sfc", "0123456789", 413},
	} {
		w := adminRequest(t, a, http.MethodPut, tc.target, tc.body)
		if w.Code != tc.code {
			t.Errorf("%s: %d %s", tc.target, w.Code, w.Body.String())
		}
	}
	// Unknown-length bodies must be bounded too.
	r := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8080/api/roms?platform=snes&name=stream.sfc", strings.NewReader("0123456789"))
	r.ContentLength = -1
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testDashboardSession})
	r.Header.Set("X-Romsync-Request", "1")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unknown length limit: %d", w.Code)
	}
	files, err := os.ReadDir(filepath.Join(f.roms, "snes"))
	if err != nil || len(files) != 1 {
		t.Fatalf("failed uploads left files: %v %v", files, err)
	}
	if len(a.jobs.snapshot()) != 0 {
		t.Fatal("invalid upload queued metadata")
	}
}

func TestAdminConcurrentDuplicateUpload(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	a.jobs.run = func(context.Context, string, io.Writer) error { return nil }
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- adminRequest(t, a, http.MethodPut, "/api/roms?platform=snes&name=unique.sfc", "ROM").Code
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for code := range codes {
		if code == http.StatusCreated {
			created++
		} else if code != http.StatusConflict {
			t.Errorf("unexpected upload status %d", code)
		}
	}
	if created != 1 {
		t.Fatalf("%d uploads created, want exactly one", created)
	}
}

func TestAdminJobFailureAndValidation(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	a.jobs.run = func(ctx context.Context, platform string, out io.Writer) error {
		fmtText := "scraper progress\n"
		_, err := io.WriteString(out, strings.Repeat(fmtText, 5000))
		if err != nil {
			return err
		}
		return errors.New("scraper deliberately failed")
	}
	decodeResponse[MetadataJob](t, adminRequest(t, a, http.MethodPost, "/api/jobs", `{"platform":"snes"}`), http.StatusAccepted)
	jobs := waitJobs(t, a)
	if jobs[0].Status != "failed" || !strings.Contains(jobs[0].Error, "deliberately failed") || len(jobs[0].Log) > 32<<10 {
		t.Fatalf("bad failure output: %+v", jobs[0])
	}
	for _, body := range []string{`{"platform":"../escape"}`, `{"platform":"missing"}`, `{"platform":"snes","unknown":true}`, `{"platform":"snes"} {}`, strings.Repeat("x", 5000)} {
		w := adminRequest(t, a, http.MethodPost, "/api/jobs", body)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Fatalf("invalid job accepted: %s %d", body, w.Code)
		}
	}
}

func TestJobQueueCoalescingAndShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	q := newJobQueue(context.Background(), func(ctx context.Context, platform string, out io.Writer) error {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	defer q.close()
	first, err := q.enqueue("snes")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := q.enqueue("snes")
	if err != nil || first.ID == second.ID {
		t.Fatalf("running upload must queue a follow-up: %v", err)
	}
	third, err := q.enqueue("snes")
	if err != nil || third.ID != second.ID {
		t.Fatal("queued work not coalesced")
	}
	for i := range 31 {
		if _, err := q.enqueue(string(rune('a' + i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.enqueue("overflow"); !errors.Is(err, errQueueFull) {
		t.Fatalf("unbounded queue: %v", err)
	}
	q.cancel()
	<-q.done
	for _, job := range q.snapshot() {
		if job.Status != "cancelled" {
			t.Fatalf("job not cancelled: %+v", job)
		}
	}
}

func TestAdminRejectsWeakConfiguration(t *testing.T) {
	f := setup(t, 1024)
	for _, options := range []AdminOptions{
		{MaxROMBytes: 1024, Skyscraper: "x", Source: "x", Timeout: time.Minute},
		{Control: setupAdmin(t, f, 1024).control, MaxROMBytes: 0, Skyscraper: "x", Source: "x", Timeout: time.Minute},
		{Control: setupAdmin(t, f, 1024).control, MaxROMBytes: 1024, Skyscraper: "x", Source: "x", Timeout: 0},
	} {
		if admin, err := NewAdmin(f.ctx, f.s, options); err == nil {
			admin.Close()
			t.Fatal("invalid admin configuration accepted")
		}
	}
}

func TestAdminInterruptedUpload(t *testing.T) {
	f := setup(t, 1024)
	a := setupAdmin(t, f, 1024)
	r := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8080/api/roms?platform=snes&name=broken.sfc", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: testDashboardSession})
	r.Header.Set("X-Romsync-Request", "1")
	r.Body = io.NopCloser(io.MultiReader(bytes.NewBufferString("partial"), errorReader{}))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("interrupted upload accepted: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(f.roms, "snes", "broken.sfc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial upload published: %v", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("connection lost") }
