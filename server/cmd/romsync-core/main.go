package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"github.com/nlutsch/romsync/server/internal/server"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

func main() {
	_, noColor := os.LookupEnv("NO_COLOR")
	log := slog.New(newConsoleHandler(os.Stderr, term.IsTerminal(int(os.Stderr.Fd())) && !noColor))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], log); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Error("romsync-core stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log *slog.Logger) error {
	command := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	defaults := server.DefaultSettings(filepath.Dir(executable))
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "romsync.json", "configuration file (defaults to romsync.json in the working directory)")
	state := flags.String("state", defaults.StatePath, "private state directory, outside ROM root")
	roms := flags.String("roms", "", "ROM root: <platform>/<game>")
	listen := flags.String("listen", defaults.Listen, "gRPC listen address")
	cert := flags.String("tls-cert", "", "TLS certificate")
	key := flags.String("tls-key", "", "TLS private key")
	maxSave := flags.Int64("max-save-bytes", defaults.MaxSaveBytes, "maximum save upload size")
	device := flags.String("device", "", "device ID for approve/revoke")
	platform := flags.String("platform", "", "Skyscraper platform identifier")
	scraper := flags.String("skyscraper", defaults.Skyscraper, "Skyscraper executable")
	wsl := flags.String("skyscraper-wsl", "", "Windows only: Skyscraper WSL distribution")
	source := flags.String("source", defaults.Source, "Skyscraper scraping module")
	timeout := flags.Duration("timeout", 30*time.Minute, "enrichment timeout")
	webListen := flags.String("web-listen", defaults.WebListen, "dashboard HTTP listen address")
	maxROM := flags.Int64("max-rom-bytes", defaults.MaxROMBytes, "maximum admin ROM upload size")
	username := flags.String("username", "", "username for reset-password (defaults to saved username)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	store, err := server.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	settings := store.Snapshot().Settings
	// Explicit CLI paths remain relative to the invocation directory; file paths are config-relative.
	cliPath := func(value string) string {
		if value == "" {
			return ""
		}
		abs, pathErr := filepath.Abs(value)
		if pathErr != nil {
			err = errors.Join(err, pathErr)
		}
		return abs
	}
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "state":
			settings.StatePath = cliPath(*state)
		case "roms":
			settings.ROMPath = cliPath(*roms)
		case "listen":
			settings.Listen = *listen
		case "web-listen":
			settings.WebListen = *webListen
		case "tls-cert":
			settings.TLSCert = cliPath(*cert)
		case "tls-key":
			settings.TLSKey = cliPath(*key)
		case "max-save-bytes":
			settings.MaxSaveBytes = *maxSave
		case "max-rom-bytes":
			settings.MaxROMBytes = *maxROM
		case "skyscraper":
			settings.Skyscraper = *scraper
		case "skyscraper-wsl":
			settings.WSLDistribution = *wsl
		case "source":
			settings.Source = *source
		case "timeout":
			settings.TimeoutSeconds = int64(*timeout / time.Second)
		}
	})
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "skyscraper" && settings.WSLDistribution == "" && strings.ContainsAny(settings.Skyscraper, `/\`) {
			settings.Skyscraper = cliPath(settings.Skyscraper)
		}
	})
	if err != nil {
		return err
	}
	switch command {
	case "reset-password":
		return resetPassword(store, *username, os.Stdin, os.Stdout)
	case "devices", "approve", "revoke":
		root, err := server.OpenState(settings.StatePath)
		if err != nil {
			return err
		}
		defer root.Close()
		if command != "devices" {
			return server.Approve(root, *device, command == "approve")
		}
		devices, err := server.Devices(root)
		if err != nil {
			return err
		}
		for _, d := range devices {
			fmt.Printf("%s\tapproved=%t\tprofile=%s\t%s\n", d.ID, d.Approved, pb.Profile(d.Profile), d.Name)
		}
		return nil
	case "enrich":
		if settings.TimeoutSeconds <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
		if settings.ROMPath == "" {
			return fmt.Errorf("configure a ROM path or supply -roms")
		}
		ctx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
		defer cancel()
		return server.Enrich(ctx, settings.ROMPath, *platform,
			server.ScraperOptions{Binary: settings.Skyscraper, Source: settings.Source, WSLDistribution: settings.WSLDistribution}, os.Stdout)
	case "serve":
	default:
		return fmt.Errorf("unknown command %q (serve, devices, approve, revoke, enrich, reset-password)", command)
	}
	// Invalid explicit listener flags should fail rather than unexpectedly opening another address.
	if err := server.ValidateSettings(&settings, store.Path(), false); err != nil {
		// Saved filesystem failures are recoverable through the dashboard; CLI mistakes are not.
		if flags.NFlag() > 0 {
			explicit := false
			flags.Visit(func(f *flag.Flag) {
				if f.Name != "config" {
					explicit = true
				}
			})
			if explicit {
				return err
			}
		}
		log.Error("saved settings need correction in dashboard", "error", err)
	}
	store.Override(settings)
	control, err := server.NewControl(store, log)
	if err != nil {
		return err
	}
	return serveManaged(ctx, store, control, log)
}

func resetPassword(store *server.ConfigStore, username string, input *os.File, output io.Writer) error {
	config := store.Snapshot()
	if config.Auth.Mode == "" {
		return fmt.Errorf("complete first-run setup before resetting the password")
	}
	if username == "" {
		username = config.Auth.Username
	}
	if username == "" {
		return fmt.Errorf("supply -username to create a login for an anonymous installation")
	}
	fmt.Fprintln(output, "Stop the server before resetting its password. No existing password is required.")
	reader := bufio.NewReader(input)
	read := func(prompt string) (string, error) {
		fmt.Fprint(output, prompt)
		if term.IsTerminal(int(input.Fd())) {
			value, err := term.ReadPassword(int(input.Fd()))
			fmt.Fprintln(output)
			return string(value), err
		}
		value, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), nil
	}
	password, err := read("New password (at least 12 characters): ")
	if err != nil {
		return err
	}
	confirmation, err := read("Confirm password: ")
	if err != nil {
		return err
	}
	if password != confirmation {
		return fmt.Errorf("passwords do not match")
	}
	auth, err := server.MakeLogin(username, password)
	if err != nil {
		return err
	}
	if err := store.SaveAuth(auth, false, nil); err != nil {
		return err
	}
	fmt.Fprintln(output, "Login saved. Start the server again; all previous sessions are invalid.")
	return nil
}

type runningServices struct {
	app      *server.Server
	admin    *server.Admin
	rpc      *grpc.Server
	listener net.Listener
	cancel   context.CancelFunc
	watchErr chan error
	serveErr chan error
}

func startServices(ctx context.Context, config server.Settings, store *server.ConfigStore, control *server.Control, log *slog.Logger) (*runningServices, error) {
	if err := server.ValidateSettings(&config, store.Path(), true); err != nil {
		return nil, err
	}
	log.Info("initializing device sync", "status", "loading cached ROM library")
	app, err := server.NewCachedContext(ctx, config.ROMPath, config.StatePath, config.MaxSaveBytes, log)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		app.Close()
		return nil, err
	}
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(server.ChunkSize + 4096), grpc.MaxSendMsgSize(32 << 20), grpc.MaxConcurrentStreams(32)}
	if config.TLSCert != "" {
		tls, err := credentials.NewServerTLSFromFile(config.TLSCert, config.TLSKey)
		if err != nil {
			listener.Close()
			app.Close()
			return nil, err
		}
		options = append(options, grpc.Creds(tls))
	}
	ctx, cancel := context.WithCancel(ctx)
	admin, err := server.NewAdmin(ctx, app, server.AdminOptions{Control: control, MaxROMBytes: config.MaxROMBytes,
		Skyscraper: config.Skyscraper, WSLDistribution: config.WSLDistribution, Source: config.Source,
		Timeout: time.Duration(config.TimeoutSeconds) * time.Second})
	if err != nil {
		cancel()
		listener.Close()
		app.Close()
		return nil, err
	}
	rpc := grpc.NewServer(options...)
	pb.RegisterRomSyncServer(rpc, app)
	reflection.Register(rpc)
	runtime := &runningServices{app: app, admin: admin, rpc: rpc, listener: listener, cancel: cancel,
		watchErr: make(chan error, 1), serveErr: make(chan error, 1)}
	go func() { runtime.watchErr <- app.Watch(ctx) }()
	go func() { runtime.serveErr <- rpc.Serve(listener) }()
	log.Info("romsync-core listening", "address", listener.Addr().String(), "tls", config.TLSCert != "")
	return runtime, nil
}

func (s *runningServices) stop() error {
	s.cancel()
	s.admin.Close()
	done := make(chan struct{})
	go func() { s.rpc.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.rpc.Stop()
		<-done
	}
	watchErr, serveErr := <-s.watchErr, <-s.serveErr
	if errors.Is(serveErr, grpc.ErrServerStopped) {
		serveErr = nil
	}
	return errors.Join(watchErr, serveErr, s.app.Close())
}

func serveManaged(ctx context.Context, store *server.ConfigStore, control *server.Control, log *slog.Logger) error {
	address := store.Snapshot().Settings.WebListen
	for {
		config := store.Snapshot()
		if err := server.ValidateWebListen(config.Settings.WebListen); err != nil {
			return err
		}
		var bindErr error
		webListener, err := net.Listen("tcp", config.Settings.WebListen)
		if err != nil {
			bindErr = fmt.Errorf("new dashboard address unavailable: %w", err)
			if address == config.Settings.WebListen {
				return bindErr
			}
			webListener, err = net.Listen("tcp", address)
			if err != nil {
				return errors.Join(bindErr, err)
			}
		}
		address = webListener.Addr().String()
		if config.Auth.Mode != "" {
			control.Detach()
		}
		web := &http.Server{Handler: control, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Minute, WriteTimeout: 30 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
		webErr := make(chan error, 1)
		go func() { webErr <- web.Serve(webListener) }()
		log.Info("admin dashboard listening", "url", "http://"+address, "config", store.Path(), "setup_required", config.Auth.Mode == "")
		var runtime *runningServices
		var runtimeErr error
		if config.Auth.Mode != "" {
			runtime, runtimeErr = startServices(ctx, config.Settings, store, control, log)
		}
		applied := config.Settings
		if bindErr != nil {
			applied.WebListen = address
		}
		if runtime != nil {
			control.SetRuntime(runtime.admin, applied, errors.Join(bindErr, runtimeErr))
		} else {
			control.SetRuntime(nil, applied, errors.Join(bindErr, runtimeErr))
		}
		var watchErr, serveErr <-chan error
		if runtime != nil {
			watchErr, serveErr = runtime.watchErr, runtime.serveErr
		}
		var result error
		webFinished := false
		select {
		case <-ctx.Done():
		case <-control.Restarts():
		case result = <-webErr:
			webFinished = true
		case result = <-watchErr:
			// Return consumed results to the buffered channel for stop() to collect.
			runtime.watchErr <- result
			if result == nil {
				result = fmt.Errorf("filesystem watcher stopped unexpectedly")
			}
		case result = <-serveErr:
			runtime.serveErr <- result
			if result == nil {
				result = fmt.Errorf("gRPC server stopped unexpectedly")
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := web.Shutdown(shutdownCtx); err != nil {
			log.Warn("closing active dashboard connections during restart", "error", err)
			result = errors.Join(result, web.Close())
		}
		cancel()
		if !webFinished {
			if err := <-webErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
				result = errors.Join(result, err)
			}
		}
		control.Detach()
		if runtime != nil {
			result = errors.Join(result, runtime.stop())
		}
		if result != nil {
			return result
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}
