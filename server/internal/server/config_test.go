package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig(t *testing.T) (*ConfigStore, Settings) {
	t.Helper()
	base := t.TempDir()
	store, err := LoadConfig(filepath.Join(base, "romsync.json"))
	if err != nil {
		t.Fatal(err)
	}
	settings := store.Snapshot().Settings
	settings.ROMPath = filepath.Join(base, "roms")
	if err := os.Mkdir(settings.ROMPath, 0755); err != nil {
		t.Fatal(err)
	}
	return store, settings
}

func TestConfigurationPersistenceAndValidation(t *testing.T) {
	store, settings := testConfig(t)
	auth, err := MakeLogin("admin", "a-long-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(auth, true, &settings); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err == nil {
		t.Fatal("setup repeated")
	}
	content, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "a-long-test-password") {
		t.Fatal("plaintext password persisted")
	}
	reloaded, err := LoadConfig(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Snapshot() != store.Snapshot() {
		t.Fatal("configuration did not round-trip")
	}
	settings.MaxROMBytes = 5000
	if err := reloaded.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if reloaded.Snapshot().Auth != auth {
		t.Fatal("settings overwrote credentials")
	}
	before, _ := os.ReadFile(store.Path())
	invalid := []Settings{}
	for _, edit := range []func(*Settings){
		func(s *Settings) { s.ROMPath = "" },
		func(s *Settings) { s.StatePath = filepath.Join(s.ROMPath, "state") },
		func(s *Settings) { s.StatePath = "" },
		func(s *Settings) { s.ROMPath = filepath.Dir(store.Path()) },
		func(s *Settings) { s.WebListen = "0.0.0.0:8080" },
		func(s *Settings) { s.WebListen = "127.0.0.1:bad" },
		func(s *Settings) { s.DashboardURL = "http://romsync.example" },
		func(s *Settings) { s.DashboardURL = "https://romsync.example/path" },
		func(s *Settings) { s.Listen = "0.0.0.0:7777" },
		func(s *Settings) { s.TLSCert = "missing.pem" },
		func(s *Settings) { s.MaxROMBytes = 0 },
		func(s *Settings) { s.MaxSaveBytes = 1 << 41 },
		func(s *Settings) { s.TimeoutSeconds = 0 },
		func(s *Settings) { s.TimeoutSeconds = 86401 },
		func(s *Settings) { s.Skyscraper = " " },
		func(s *Settings) { s.Source = "" },
	} {
		bad := settings
		edit(&bad)
		invalid = append(invalid, bad)
	}
	for _, bad := range invalid {
		if err := reloaded.SaveSettings(bad); err == nil {
			t.Fatalf("invalid settings accepted: %+v", bad)
		}
	}
	after, _ := os.ReadFile(store.Path())
	if string(before) != string(after) {
		t.Fatal("invalid update changed configuration")
	}
	relative := settings
	relative.ROMPath = "roms"
	relative.StatePath = "private"
	if err := reloaded.SaveSettings(relative); err != nil {
		t.Fatal(err)
	}
	if reloaded.Snapshot().Settings.ROMPath != settings.ROMPath || !filepath.IsAbs(reloaded.Snapshot().Settings.StatePath) {
		t.Fatal("relative paths are not config-relative")
	}
}

func TestInvalidConfigFailsExplicitly(t *testing.T) {
	store, _ := testConfig(t)
	for _, data := range []string{
		`{`, `{"version":99}`, `{"version":1,"unexpected":true}`,
		`{"version":1} {}`, `{"version":1,"auth":{"mode":"login","password_hash":"bad"}}`,
		`{"version":1,"auth":{"mode":"anonymous","username":"leaked"}}`,
	} {
		if err := os.WriteFile(store.Path(), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(store.Path()); err == nil {
			t.Fatalf("invalid configuration accepted: %s", data)
		}
	}
}

func TestLoginHashing(t *testing.T) {
	one, err := MakeLogin("admin", "long-password-test")
	if err != nil {
		t.Fatal(err)
	}
	two, err := MakeLogin("admin", "long-password-test")
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !verifyLogin(one, "admin", "long-password-test") || verifyLogin(one, "wrong", "long-password-test") || verifyLogin(one, "admin", "wrong") {
		t.Fatal("password verification or salt invalid")
	}
	for _, pair := range [][2]string{{"", "long-password-test"}, {" admin", "long-password-test"}, {"admin", "short"}, {"admin", strings.Repeat("\u00e9", 6)}, {"admin", strings.Repeat("x", 1025)}} {
		if _, err := MakeLogin(pair[0], pair[1]); err == nil {
			t.Fatal("weak/invalid credentials accepted")
		}
	}
}

func TestConfigurationWriteFailurePreservesMemory(t *testing.T) {
	store, settings := testConfig(t)
	if err := store.SaveAuth(DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	if err := os.Remove(store.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.Path(), 0700); err != nil {
		t.Fatal(err)
	}
	settings.MaxROMBytes++
	if err := store.SaveSettings(settings); err == nil {
		t.Fatal("disk write failure hidden")
	}
	if store.Snapshot() != before {
		t.Fatal("failed save changed live config")
	}
}

func jsonBody(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
