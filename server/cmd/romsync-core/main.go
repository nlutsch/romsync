package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"github.com/nlutsch/romsync/server/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	state := flags.String("state", "./data", "private state directory, outside ROM root")
	roms := flags.String("roms", "./roms", "ROM root: <platform>/<game>")
	listen := flags.String("listen", "127.0.0.1:7777", "gRPC listen address")
	cert := flags.String("tls-cert", "", "optional TLS certificate (requires tls-key)")
	key := flags.String("tls-key", "", "optional TLS private key")
	maxSave := flags.Int64("max-save-bytes", 64<<20, "maximum save upload size")
	device := flags.String("device", "", "device ID for approve/revoke")
	platform := flags.String("platform", "", "Skyscraper platform identifier")
	scraper := flags.String("skyscraper", "Skyscraper", "Skyscraper executable")
	wslDistribution := flags.String("skyscraper-wsl", "", "Windows only: run Skyscraper in this WSL distribution, e.g. Ubuntu")
	source := flags.String("source", "screenscraper", "Skyscraper scraping module")
	timeout := flags.Duration("timeout", 30*time.Minute, "enrichment timeout")
	webListen := flags.String("web-listen", "", "optional loopback admin HTTP address, e.g. 127.0.0.1:8080 (requires ROMSYNC_ADMIN_TOKEN)")
	maxROM := flags.Int64("max-rom-bytes", 2<<30, "maximum admin ROM upload size")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	switch command {
	case "devices", "approve", "revoke":
		root, err := server.OpenState(*state)
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
		if *timeout <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		return server.Enrich(ctx, *roms, *platform, server.ScraperOptions{Binary: *scraper, Source: *source, WSLDistribution: *wslDistribution}, os.Stdout)
	case "serve":
	default:
		return fmt.Errorf("unknown command %q (serve, devices, approve, revoke, enrich)", command)
	}
	if (*cert == "") != (*key == "") {
		return fmt.Errorf("tls-cert and tls-key must be specified together")
	}
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(server.ChunkSize + 4096), grpc.MaxSendMsgSize(32 << 20), grpc.MaxConcurrentStreams(32)}
	if *cert != "" {
		tls, err := credentials.NewServerTLSFromFile(*cert, *key)
		if err != nil {
			return err
		}
		options = append(options, grpc.Creds(tls))
	} else {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("plaintext gRPC must listen on a loopback IP; configure TLS for remote access")
		}
	}
	app, err := server.New(*roms, *state, *maxSave, log)
	if err != nil {
		return err
	}
	defer app.Close()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	rpc := grpc.NewServer(options...)
	pb.RegisterRomSyncServer(rpc, app)
	reflection.Register(rpc)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var web *http.Server
	var admin *server.Admin
	webErr := make(chan error, 1)
	if *webListen != "" {
		host, _, err := net.SplitHostPort(*webListen)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("admin HTTP must listen on a loopback IP; use Nginx HTTPS for remote access")
		}
		admin, err = server.NewAdmin(ctx, app, server.AdminOptions{Token: os.Getenv("ROMSYNC_ADMIN_TOKEN"), MaxROMBytes: *maxROM, Skyscraper: *scraper, WSLDistribution: *wslDistribution, Source: *source, Timeout: *timeout})
		if err != nil {
			return err
		}
		defer admin.Close()
		webListener, err := net.Listen("tcp", *webListen)
		if err != nil {
			return err
		}
		defer webListener.Close()
		web = &http.Server{Handler: admin, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Minute, WriteTimeout: 30 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
		go func() { webErr <- web.Serve(webListener) }()
		log.Info("admin dashboard listening", "address", webListener.Addr().String())
	}
	watchErr := make(chan error, 1)
	go func() { watchErr <- app.Watch(ctx) }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- rpc.Serve(listener) }()
	log.Info("romsync-core listening", "address", listener.Addr().String(), "tls", *cert != "")
	var result error
	watcherFinished := false
	webFinished := false
	select {
	case <-ctx.Done():
	case result = <-watchErr:
		watcherFinished = true
	case result = <-serveErr:
	case result = <-webErr:
		webFinished = true
	}
	cancel()
	if web != nil {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		if err := web.Shutdown(shutdownCtx); err != nil {
			log.Error("admin shutdown timed out", "error", err)
			if closeErr := web.Close(); closeErr != nil {
				result = errors.Join(result, closeErr)
			}
		}
		stop()
		if !webFinished {
			if err := <-webErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
				result = errors.Join(result, err)
			}
		}
	}
	if !watcherFinished {
		if err := <-watchErr; err != nil {
			result = errors.Join(result, err)
		}
	}
	done := make(chan struct{})
	go func() { rpc.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		rpc.Stop()
		<-done
	}
	if result == grpc.ErrServerStopped || result == http.ErrServerClosed {
		return nil
	}
	return result
}
