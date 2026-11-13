package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"github.com/nlutsch/romsync/server/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

type startupGate struct {
	slog.Handler
	entered chan struct{}
	release chan struct{}
	message string
}

func (h startupGate) Handle(ctx context.Context, record slog.Record) error {
	message := h.message
	if message == "" {
		message = "initializing device sync"
	}
	if record.Message == message {
		close(h.entered)
		<-h.release
	}
	return h.Handler.Handle(ctx, record)
}

func TestDashboardAvailableDuringInitialization(t *testing.T) {
	dir := t.TempDir()
	roms := filepath.Join(dir, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	config := lifecycleConfig(t, dir, roms)
	address := freeAddress(t)
	gate := startupGate{slog.NewTextHandler(io.Discard, nil), make(chan struct{}), make(chan struct{}), ""}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-config", config, "-web-listen", address, "-listen", "127.0.0.1:0"}, slog.New(gate))
	}()
	defer func() {
		cancel()
		close(gate.release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("startup cancellation did not stop the server")
		}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("service initialization did not start")
	}
	status := httpConfig(t, "GET", address, "/api/status", nil, 200)
	if string(status["ready"]) != "false" || string(status["restarting"]) != "true" {
		t.Fatal("dashboard did not report initialization in progress")
	}
	httpConfig(t, "GET", address, "/api/settings", nil, 200)
}

func TestDashboardCatalogAvailableBeforeBackgroundScan(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "cold"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			roms := filepath.Join(dir, "roms")
			if err := os.MkdirAll(filepath.Join(roms, "SFC"), 0755); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(roms, "SFC", "game.sfc")
			if err := os.WriteFile(filename, []byte("original ROM"), 0644); err != nil {
				t.Fatal(err)
			}
			config := lifecycleConfig(t, dir, roms)
			var checksum string
			if cached {
				app, err := server.New(roms, filepath.Join(dir, "data"), 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				if err := app.Close(); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "data", "catalog.json"))
				if err != nil {
					t.Fatal(err)
				}
				var cache struct {
					Catalog *pb.Catalog `json:"catalog"`
				}
				if err := json.Unmarshal(data, &cache); err != nil {
					t.Fatal(err)
				}
				checksum = cache.Catalog.Entries[0].Sha256
				if err := os.WriteFile(filename, []byte("changed on disk"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			address := freeAddress(t)
			gate := startupGate{slog.NewTextHandler(io.Discard, nil), make(chan struct{}), make(chan struct{}), "reconciling ROM library in background"}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- run(ctx, []string{"-config", config, "-web-listen", address, "-listen", "127.0.0.1:0"}, slog.New(gate))
			}()
			defer func() {
				cancel()
				close(gate.release)
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not shut down")
				}
			}()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("background reconciliation did not start")
			}
			deadline := time.Now().Add(3 * time.Second)
			for string(httpConfig(t, "GET", address, "/api/status", nil, 200)["ready"]) != "true" {
				if time.Now().After(deadline) {
					t.Fatal("services waited for background hashing")
				}
				time.Sleep(10 * time.Millisecond)
			}
			response := httpConfig(t, "GET", address, "/api/library", nil, 200)
			if string(response["indexing"]) != "true" && !cached {
				t.Fatal("cold startup did not report indexing")
			}
			var games []struct {
				ID     string `json:"id"`
				SHA256 string `json:"sha256"`
			}
			if err := json.Unmarshal(response["games"], &games); err != nil {
				t.Fatal(err)
			}
			if cached && (len(games) != 1 || games[0].ID != "snes/game.sfc" || games[0].SHA256 != checksum || string(response["indexing"]) != "false") {
				t.Fatalf("cached library unavailable while hashing was blocked: %s", response["games"])
			}
			if !cached && len(games) != 0 {
				t.Fatal("cold startup reported unscanned games")
			}
		})
	}
}

func TestServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	roms := filepath.Join(dir, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	configPath := lifecycleConfig(t, dir, roms)
	go func() {
		done <- run(ctx, []string{"serve", "-config", configPath, "-listen", address, "-web-listen", "127.0.0.1:0"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	requestCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	reply, err := pb.NewRomSyncClient(conn).RegisterDevice(requestCtx, &pb.RegisterDeviceRequest{Name: "smoke test", Profile: pb.Profile_BATOCERA}, grpc.WaitForReady(true))
	if err != nil {
		t.Fatal(err)
	}
	if reply.DeviceId == "" || reply.Token == "" {
		t.Fatal("registration failed")
	}
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(requestCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseSend()
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		t.Fatal(err)
	}
	services, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, service := range services.GetListServicesResponse().GetService() {
		if service.Name == "romsync.v1.RomSync" {
			found = true
		}
	}
	if !found {
		t.Fatalf("RomSync missing from reflection response: %v", services)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: "romsync.v1.RomSync"},
	}); err != nil {
		t.Fatal(err)
	}
	schema, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.GetFileDescriptorResponse().GetFileDescriptorProto()) == 0 {
		t.Fatalf("reflection returned no schema: %v", schema)
	}
}

func TestInvalidOptions(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{
		{"serve", "-listen", "0.0.0.0:bad"},
		{"serve", "-tls-cert", "only-cert"},
		{"enrich", "-timeout", "0s"},
		{"unknown"},
		{""},
	} {
		if err := run(context.Background(), args, log); err == nil {
			t.Fatalf("accepted invalid options: %v", args)
		}
	}
}

func lifecycleConfig(t *testing.T, dir, roms string) string {
	t.Helper()
	name := filepath.Join(dir, "romsync.json")
	store, err := server.LoadConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	settings := server.DefaultSettings(dir)
	settings.ROMPath = roms
	if err := store.SaveAuth(server.DashboardAuth{Mode: "anonymous"}, true, &settings); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestAdminServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	roms := filepath.Join(dir, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	configPath := lifecycleConfig(t, dir, roms)
	go func() {
		done <- run(ctx, []string{"serve", "-config", configPath, "-listen", "127.0.0.1:0", "-web-listen", address}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("dashboard not served: %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/library", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated API not responsive: %d", response.StatusCode)
	}
}

func TestAdminListenValidation(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, address := range []string{"0.0.0.0:65536", "not-an-address", ""} {
		err := run(context.Background(), []string{"serve", "-config", filepath.Join(t.TempDir(), "romsync.json"), "-roms", dir, "-state", filepath.Join(t.TempDir(), "state"), "-listen", "127.0.0.1:0", "-web-listen", address}, log)
		if err == nil {
			t.Fatalf("invalid admin listener accepted: %s", address)
		}
	}
}
