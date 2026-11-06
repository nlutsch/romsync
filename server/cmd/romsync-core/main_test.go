package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

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
	go func() {
		done <- run(ctx, []string{"serve", "-listen", address, "-roms", roms, "-state", filepath.Join(dir, "state")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
		{"serve", "-listen", "0.0.0.0:7777"},
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

func TestAdminServeLifecycle(t *testing.T) {
	t.Setenv("ROMSYNC_ADMIN_TOKEN", "test-only-lifecycle-admin-token-value")
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
	go func() {
		done <- run(ctx, []string{"serve", "-listen", "127.0.0.1:0", "-web-listen", address, "-roms", roms, "-state", filepath.Join(dir, "state")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ROMSYNC_ADMIN_TOKEN"))
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
	for _, address := range []string{"0.0.0.0:8080", "not-an-address", "127.0.0.1:0"} {
		// The loopback case must fail because no valid token is configured.
		t.Setenv("ROMSYNC_ADMIN_TOKEN", "")
		err := run(context.Background(), []string{"serve", "-roms", dir, "-state", filepath.Join(t.TempDir(), "state"), "-listen", "127.0.0.1:0", "-web-listen", address}, log)
		if err == nil {
			t.Fatalf("invalid admin listener accepted: %s", address)
		}
	}
}
