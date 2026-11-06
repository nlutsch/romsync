package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/credentials"
)

type Settings struct {
	ROMPath         string `json:"rom_path"`
	StatePath       string `json:"state_path"`
	Listen          string `json:"listen"`
	WebListen       string `json:"web_listen"`
	DashboardURL    string `json:"dashboard_url"`
	TLSCert         string `json:"tls_cert"`
	TLSKey          string `json:"tls_key"`
	MaxSaveBytes    int64  `json:"max_save_bytes"`
	MaxROMBytes     int64  `json:"max_rom_bytes"`
	Skyscraper      string `json:"skyscraper"`
	WSLDistribution string `json:"skyscraper_wsl"`
	Source          string `json:"source"`
	TimeoutSeconds  int64  `json:"timeout_seconds"`
}

type DashboardAuth struct {
	Mode         string `json:"mode"`
	Username     string `json:"username,omitempty"`
	Salt         string `json:"salt,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
}

type FileConfig struct {
	Version  int           `json:"version"`
	Settings Settings      `json:"settings"`
	Auth     DashboardAuth `json:"auth"`
}

type ConfigStore struct {
	mu   sync.Mutex
	path string
	data FileConfig
}

func DefaultSettings(base string) Settings {
	return Settings{StatePath: filepath.Join(base, "data"), Listen: "127.0.0.1:7777",
		WebListen: "127.0.0.1:8080", MaxSaveBytes: 64 << 20, MaxROMBytes: 2 << 30,
		Skyscraper: "Skyscraper", Source: "screenscraper", TimeoutSeconds: 1800}
}

func LoadConfig(name string) (*ConfigStore, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	store := &ConfigStore{path: name, data: FileConfig{Version: 1, Settings: DefaultSettings(filepath.Dir(name))}}
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&store.data); err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("configuration must contain one JSON object")
	}
	if store.data.Version != 1 {
		return nil, fmt.Errorf("unsupported configuration version %d", store.data.Version)
	}
	if err := validateAuth(store.data.Auth); err != nil {
		return nil, err
	}
	normalizeSettingsPaths(&store.data.Settings, filepath.Dir(store.path))
	return store, nil
}

func (s *ConfigStore) Path() string { return s.path }
func (s *ConfigStore) Snapshot() FileConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// Override is used only for explicit command-line flags; web saves persist their effective values.
func (s *ConfigStore) Override(settings Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Settings = settings
}

func (s *ConfigStore) saveLocked(next FileConfig) error {
	root, err := os.OpenRoot(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer root.Close()
	if err := writeJSON(root, filepath.Base(s.path), next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *ConfigStore) SaveSettings(settings Settings) error {
	if err := ValidateSettings(&settings, s.path, true); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	next.Settings = settings
	return s.saveLocked(next)
}

func (s *ConfigStore) SaveAuth(auth DashboardAuth, initial bool, settings *Settings) error {
	if err := validateAuth(auth); err != nil {
		return err
	}
	if settings != nil {
		if err := ValidateSettings(settings, s.path, true); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if initial && s.data.Auth.Mode != "" {
		return fmt.Errorf("setup has already been completed")
	}
	next := s.data
	next.Auth = auth
	if settings != nil {
		next.Settings = *settings
	}
	return s.saveLocked(next)
}

func MakeLogin(username, password string) (DashboardAuth, error) {
	if strings.TrimSpace(username) != username || len(username) == 0 || len(username) > 128 || strings.ContainsAny(username, "\r\n\x00") {
		return DashboardAuth{}, fmt.Errorf("username must be 1-128 characters with no surrounding whitespace")
	}
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 12 || len(password) > 1024 {
		return DashboardAuth{}, fmt.Errorf("password must contain at least 12 characters and at most 1024 UTF-8 bytes")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return DashboardAuth{}, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if err != nil {
		return DashboardAuth{}, err
	}
	return DashboardAuth{Mode: "login", Username: username, Salt: hex.EncodeToString(salt), PasswordHash: hex.EncodeToString(hash)}, nil
}

func validateAuth(auth DashboardAuth) error {
	switch auth.Mode {
	case "":
		if auth != (DashboardAuth{}) {
			return fmt.Errorf("invalid unconfigured authentication")
		}
	case "anonymous":
		if auth.Username != "" || auth.Salt != "" || auth.PasswordHash != "" {
			return fmt.Errorf("anonymous authentication cannot include credentials")
		}
	case "login":
		if auth.Username == "" || !validDigest(auth.Salt) || !validDigest(auth.PasswordHash) {
			return fmt.Errorf("invalid stored login credentials")
		}
	default:
		return fmt.Errorf("unsupported authentication mode")
	}
	return nil
}

func verifyLogin(auth DashboardAuth, username, password string) bool {
	if len(password) > 1024 {
		return false
	}
	salt, _ := hex.DecodeString(auth.Salt) // Validated when loading or saving the configuration.
	expected, _ := hex.DecodeString(auth.PasswordHash)
	hash, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return err == nil && subtle.ConstantTimeCompare(hash, expected) == 1 && username == auth.Username
}

func loopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("port must be a number between 0 and 65535")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("address must use a loopback IP")
	}
	return nil
}

func ValidateWebListen(address string) error {
	if err := loopbackAddress(address); err != nil {
		return fmt.Errorf("admin HTTP must use a loopback IP: %w", err)
	}
	return nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))))
}

// Resolve existing ancestors too, so nonexistent state directories cannot bypass symlink checks.
func canonicalPath(name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(name)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(name)
	if parent == name {
		return "", err
	}
	resolved, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(name)), nil
}

func normalizeSettingsPaths(settings *Settings, base string) {
	for _, field := range []*string{&settings.ROMPath, &settings.StatePath, &settings.TLSCert, &settings.TLSKey} {
		if *field != "" {
			if !filepath.IsAbs(*field) {
				*field = filepath.Join(base, *field)
			}
			*field = filepath.Clean(*field)
		}
	}
	if settings.WSLDistribution == "" && strings.ContainsAny(settings.Skyscraper, `/\`) && !filepath.IsAbs(settings.Skyscraper) {
		settings.Skyscraper = filepath.Join(base, settings.Skyscraper)
	}
}

func ValidateSettings(settings *Settings, configPath string, requireROM bool) error {
	normalizeSettingsPaths(settings, filepath.Dir(configPath))
	if settings.ROMPath == "" {
		if requireROM {
			return fmt.Errorf("ROM directory is required")
		}
	} else {
		roms, err := canonicalPath(settings.ROMPath)
		if err != nil {
			return fmt.Errorf("ROM path: %w", err)
		}
		info, err := os.Stat(roms)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("ROM path must be an existing directory")
		}
		for _, private := range []string{settings.StatePath, configPath, settings.TLSKey} {
			if private == "" {
				continue
			}
			name, err := canonicalPath(private)
			if err != nil {
				return err
			}
			if within(roms, name) {
				return fmt.Errorf("state, configuration, and TLS private key must be outside the ROM directory")
			}
		}
	}
	if settings.StatePath == "" {
		return fmt.Errorf("state directory is required")
	}
	if settings.WSLDistribution != "" && runtime.GOOS != "windows" {
		return fmt.Errorf("Skyscraper WSL mode is supported only on Windows")
	}
	if info, err := os.Stat(settings.StatePath); err == nil && !info.IsDir() {
		return fmt.Errorf("state path must be a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state directory: %w", err)
	}
	if err := loopbackAddress(settings.WebListen); err != nil {
		return fmt.Errorf("admin HTTP: %w; use Nginx HTTPS for remote access", err)
	}
	if settings.DashboardURL != "" {
		u, err := url.Parse(settings.DashboardURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("public dashboard URL must be an HTTPS origin, e.g. https://romsync.example.com")
		}
		settings.DashboardURL = strings.TrimSuffix(u.String(), "/")
	}
	if _, port, err := net.SplitHostPort(settings.Listen); err != nil {
		return fmt.Errorf("gRPC listen address: %w", err)
	} else if number, err := strconv.Atoi(port); err != nil || number < 0 || number > 65535 {
		return fmt.Errorf("gRPC port must be a number between 0 and 65535")
	}
	if (settings.TLSCert == "") != (settings.TLSKey == "") {
		return fmt.Errorf("TLS certificate and key must be specified together")
	}
	if settings.TLSCert == "" {
		if err := loopbackAddress(settings.Listen); err != nil {
			return fmt.Errorf("plaintext gRPC: %w; configure TLS for remote access", err)
		}
	} else if _, err := credentials.NewServerTLSFromFile(settings.TLSCert, settings.TLSKey); err != nil {
		return fmt.Errorf("TLS configuration: %w", err)
	}
	if settings.MaxSaveBytes <= 0 || settings.MaxSaveBytes > 1<<40 || settings.MaxROMBytes <= 0 || settings.MaxROMBytes > 1<<40 {
		return fmt.Errorf("upload limits must be between 1 byte and 1 TiB")
	}
	if settings.TimeoutSeconds <= 0 || settings.TimeoutSeconds > int64((24*time.Hour)/time.Second) {
		return fmt.Errorf("metadata timeout must be between 1 second and 24 hours")
	}
	if strings.TrimSpace(settings.Skyscraper) == "" || strings.TrimSpace(settings.Source) == "" {
		return fmt.Errorf("Skyscraper executable and source are required")
	}
	return nil
}
