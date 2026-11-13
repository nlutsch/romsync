package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nlutsch/romsync/server/internal/server"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func launchConfigured(t *testing.T, name string, extra ...string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	args := []string{"serve"}
	if name != "" {
		args = append(args, "-config", name)
	}
	args = append(args, extra...)
	go func() { done <- run(ctx, args, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("managed server did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestDefaultConfigUsesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Error(err)
		}
	})

	address, deviceAddress := freeAddress(t), freeAddress(t)
	stop := launchConfigured(t, "", "-web-listen", address, "-listen", deviceAddress)
	waitControl(t, address, 1, false)
	roms := filepath.Join(dir, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	settings := server.DefaultSettings(dir)
	settings.ROMPath, settings.WebListen, settings.Listen = roms, address, deviceAddress
	httpConfig(t, "POST", address, "/api/setup", map[string]any{"settings": settings, "mode": "anonymous"}, 201)
	stop()

	store, err := server.LoadConfig(filepath.Join(dir, "romsync.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Auth.Mode != "anonymous" {
		t.Fatal("default config was not saved in the working directory")
	}
}

func httpConfig(t *testing.T, method, address, path string, value any, code int) map[string]json.RawMessage {
	t.Helper()
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, "http://"+address+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Romsync-Request", "1")
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != code {
		t.Fatalf("%s %s: %d want %d: %s", method, path, response.StatusCode, code, data)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitControl(t *testing.T, address string, generation uint64, ready bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + address + "/api/status")
		if err == nil {
			var status struct {
				Generation uint64 `json:"generation"`
				Ready      bool   `json:"ready"`
				Restarting bool   `json:"restarting"`
			}
			err = json.NewDecoder(response.Body).Decode(&status)
			response.Body.Close()
			if err == nil && status.Generation >= generation && status.Ready == ready && !status.Restarting {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("dashboard did not reach expected runtime state", address, generation, ready)
}

func TestFirstRunAndWebManagedRestart(t *testing.T) {
	base := t.TempDir()
	name := filepath.Join(base, "romsync.json")
	address, deviceAddress := freeAddress(t), freeAddress(t)
	stop := launchConfigured(t, name, "-web-listen", address, "-listen", deviceAddress)
	waitControl(t, address, 1, false)
	status := httpConfig(t, "GET", address, "/api/status", nil, 200)
	if string(status["configured"]) != "false" {
		t.Fatal("first run skipped setup")
	}
	settings := server.DefaultSettings(base)
	settings.WebListen, settings.Listen = address, deviceAddress
	settings.ROMPath = filepath.Join(base, "roms")
	if err := os.MkdirAll(filepath.Join(settings.ROMPath, "snes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.ROMPath, "snes", "old.sfc"), []byte("synthetic"), 0644); err != nil {
		t.Fatal(err)
	}
	httpConfig(t, "POST", address, "/api/setup", map[string]any{"settings": settings, "mode": "anonymous"}, 201)
	waitControl(t, address, 2, true)
	library := httpConfig(t, "GET", address, "/api/library", nil, 200)
	if !bytes.Contains(library["games"], []byte("old.sfc")) {
		t.Fatal("initial library missing")
	}
	settings.ROMPath = filepath.Join(base, "new-roms")
	settings.MaxROMBytes = 1024
	if err := os.MkdirAll(filepath.Join(settings.ROMPath, "nes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.ROMPath, "nes", "new.nes"), []byte("synthetic"), 0644); err != nil {
		t.Fatal(err)
	}
	httpConfig(t, "PUT", address, "/api/settings", settings, 200)
	library = httpConfig(t, "GET", address, "/api/library", nil, 200)
	if !bytes.Contains(library["games"], []byte("old.sfc")) {
		t.Fatal("save changed runtime before restart")
	}
	httpConfig(t, "POST", address, "/api/restart", map[string]any{}, 202)
	waitControl(t, address, 3, true)
	library = httpConfig(t, "GET", address, "/api/library", nil, 200)
	if !bytes.Contains(library["games"], []byte("new.nes")) || bytes.Contains(library["games"], []byte("old.sfc")) || string(library["max_rom_bytes"]) != "1024" {
		t.Fatal("runtime did not apply settings")
	}
	settings.WebListen = freeAddress(t)
	httpConfig(t, "PUT", address, "/api/settings", settings, 200)
	httpConfig(t, "POST", address, "/api/restart", map[string]any{}, 202)
	waitControl(t, settings.WebListen, 4, true)
	stop()
	store, err := server.LoadConfig(name)
	if err != nil || store.Snapshot().Settings != settings {
		t.Fatal("effective settings not persisted", err)
	}
	// A normal executable restart uses only the saved file, not the original CLI flags.
	stop = launchConfigured(t, name)
	waitControl(t, settings.WebListen, 1, true)
	httpConfig(t, "GET", settings.WebListen, "/api/library", nil, 200)
	stop()
	// Missing ROM storage must not prevent administrators from fixing it in the dashboard.
	if err := os.Rename(settings.ROMPath, settings.ROMPath+"-offline"); err != nil {
		t.Fatal(err)
	}
	stop = launchConfigured(t, name)
	waitControl(t, settings.WebListen, 1, false)
	view := httpConfig(t, "GET", settings.WebListen, "/api/settings", nil, 200)
	if string(view["runtime_error"]) == `""` {
		t.Fatal("startup failure not surfaced")
	}
	httpConfig(t, "GET", settings.WebListen, "/api/library", nil, 503)
	settings.ROMPath += "-offline"
	httpConfig(t, "PUT", settings.WebListen, "/api/settings", settings, 200)
	httpConfig(t, "POST", settings.WebListen, "/api/restart", map[string]any{}, 202)
	waitControl(t, settings.WebListen, 2, true)
	stop()
}

func TestUnavailableDashboardPortFallsBack(t *testing.T) {
	base := t.TempDir()
	roms := filepath.Join(base, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	name := lifecycleConfig(t, base, roms)
	store, err := server.LoadConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	settings := store.Snapshot().Settings
	settings.WebListen, settings.Listen = freeAddress(t), freeAddress(t)
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	stop := launchConfigured(t, name)
	waitControl(t, settings.WebListen, 1, true)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	oldAddress := settings.WebListen
	settings.WebListen = occupied.Addr().String()
	httpConfig(t, "PUT", oldAddress, "/api/settings", settings, 200)
	httpConfig(t, "POST", oldAddress, "/api/restart", map[string]any{}, 202)
	waitControl(t, oldAddress, 2, true)
	view := httpConfig(t, "GET", oldAddress, "/api/settings", nil, 200)
	if string(view["restart_required"]) != "true" || string(view["runtime_error"]) == `""` {
		t.Fatal("bind fallback not reported")
	}
	stop()
}

func TestPasswordRecoveryCLI(t *testing.T) {
	base := t.TempDir()
	store, err := server.LoadConfig(filepath.Join(base, "romsync.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(server.DashboardAuth{Mode: "anonymous"}, false, nil); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(t.TempDir(), "password-input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := io.WriteString(input, "replacement-password\nreplacement-password\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := resetPassword(store, "recovered", input, &output); err != nil {
		t.Fatal(err)
	}
	reloaded, err := server.LoadConfig(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	auth := reloaded.Snapshot().Auth
	if auth.Mode != "login" || auth.Username != "recovered" || auth.PasswordHash == "" {
		t.Fatal("recovery did not persist login")
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("replacement-password")) {
		t.Fatal("plaintext recovery password persisted")
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := resetPassword(reloaded, "", input, &output); err != nil {
		t.Fatal("saved username was not reused", err)
	}
}
