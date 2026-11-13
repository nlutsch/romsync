package server

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fixture struct {
	s           *Server
	client      pb.RomSyncClient
	ctx         context.Context
	roms, state string
}

func setup(t *testing.T, maxSave int64) *fixture {
	t.Helper()
	base := t.TempDir()
	roms, state := filepath.Join(base, "roms"), filepath.Join(base, "state")
	for _, platform := range []string{"snes", "ps2"} {
		if err := os.MkdirAll(filepath.Join(roms, platform), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roms, platform, "game.rom"), []byte("ROM contents"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(roms, state, maxSave, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	rpc := grpc.NewServer(grpc.MaxRecvMsgSize(ChunkSize + 4096))
	pb.RegisterRomSyncServer(rpc, s)
	go func() {
		if err := rpc.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Error(err)
		}
	}()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(func() { cancel(); conn.Close(); rpc.Stop(); listener.Close(); s.Close() })
	return &fixture{s: s, client: pb.NewRomSyncClient(conn), ctx: ctx, roms: roms, state: state}
}

func (f *fixture) enroll(t *testing.T, profile pb.Profile, approved bool) (context.Context, string) {
	t.Helper()
	reply, err := f.client.RegisterDevice(f.ctx, &pb.RegisterDeviceRequest{Name: "test client", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		if err := Approve(f.s.state, reply.DeviceId, true); err != nil {
			t.Fatal(err)
		}
	}
	return metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+reply.Token), reply.DeviceId
}

func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func upload(t *testing.T, client pb.RomSyncClient, ctx context.Context, data []byte, base string) (*pb.SaveRevision, error) {
	t.Helper()
	stream, err := client.UploadSave(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pb.FileChunk{GameId: "snes/game.rom", BaseRevision: base, Sha256: digest(string(data))}); err != nil {
		return nil, err
	}
	for offset := 0; offset < len(data); offset += ChunkSize {
		if err := stream.Send(&pb.FileChunk{Data: data[offset:min(offset+ChunkSize, len(data))]}); err != nil {
			return nil, err
		}
	}
	return stream.CloseAndRecv()
}

func TestApprovalAndCatalog(t *testing.T) {
	f := setup(t, 1<<20)
	_, err := f.client.CheckSaveState(f.ctx, &pb.CheckSaveStateRequest{GameId: "snes/game.rom"})
	requireCode(t, err, codes.Unauthenticated)
	ctx, id := f.enroll(t, pb.Profile_TVOS, false)
	_, err = f.client.CheckSaveState(ctx, &pb.CheckSaveStateRequest{GameId: "snes/game.rom"})
	requireCode(t, err, codes.PermissionDenied)
	if err := Approve(f.s.state, id, true); err != nil {
		t.Fatal(err)
	}
	stream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries) != 1 || catalog.Entries[0].Platform != "snes" {
		t.Fatalf("tvOS catalog: %v", catalog)
	}
	_, err = f.client.CheckSaveState(ctx, &pb.CheckSaveStateRequest{GameId: "ps2/game.rom"})
	requireCode(t, err, codes.NotFound)
	entry := catalog.Entries[0]
	download, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: entry.Id, Sha256: entry.Sha256})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for {
		chunk, err := download.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Data) > ChunkSize {
			t.Fatal("unbounded chunk")
		}
		out.Write(chunk.Data)
	}
	if out.String() != "ROM contents" {
		t.Fatal(out.String())
	}
	for _, id := range []string{"../state/devices/private.json", "ps2/game.rom"} {
		stream, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: id})
		if err == nil {
			_, err = stream.Recv()
		}
		requireCode(t, err, codes.NotFound)
	}
	if err := Approve(f.s.state, id, false); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.CheckSaveState(ctx, &pb.CheckSaveStateRequest{GameId: entry.Id})
	requireCode(t, err, codes.PermissionDenied)
}

func TestSaveRoundTripConflictAndRestart(t *testing.T) {
	f := setup(t, 1<<20)
	ctx, _ := f.enroll(t, pb.Profile_BATOCERA, true)
	first := bytes.Repeat([]byte("a"), 200_000)
	r1, err := upload(t, f.client, ctx, first, "")
	if err != nil {
		t.Fatal(err)
	}
	check, err := f.client.CheckSaveState(ctx, &pb.CheckSaveStateRequest{GameId: "snes/game.rom", BaseRevision: r1.Revision, Sha256: r1.Sha256})
	if err != nil || check.State != pb.SaveState_UP_TO_DATE {
		t.Fatalf("%v %v", check, err)
	}
	second := []byte("newer save")
	r2, err := upload(t, f.client, ctx, second, r1.Revision)
	if err != nil || r2.ReplacedConflict {
		t.Fatalf("%v %v", r2, err)
	}
	check, err = f.client.CheckSaveState(ctx, &pb.CheckSaveStateRequest{GameId: "snes/game.rom", BaseRevision: r1.Revision, Sha256: digest("offline change")})
	if err != nil || check.State != pb.SaveState_CONFLICT {
		t.Fatalf("%v %v", check, err)
	}
	third := []byte("last uploaded wins")
	r3, err := upload(t, f.client, ctx, third, r1.Revision)
	if err != nil || !r3.ReplacedConflict {
		t.Fatalf("%v %v", r3, err)
	}
	stream, err := f.client.DownloadSave(ctx, &pb.DownloadSaveRequest{GameId: "snes/game.rom", Revision: r3.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data.Write(chunk.Data)
	}
	if !bytes.Equal(data.Bytes(), third) {
		t.Fatal("save mismatch")
	}
	backups, err := os.ReadDir(filepath.Join(f.state, "backups"))
	if err != nil || len(backups) != 3 {
		t.Fatalf("backups=%d err=%v", len(backups), err)
	}
	var foundFirst bool
	for _, file := range backups {
		b, err := os.ReadFile(filepath.Join(f.state, "backups", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		foundFirst = foundFirst || bytes.Equal(b, first)
	}
	if !foundFirst {
		t.Fatal("old save lost")
	}
	restarted, err := New(f.roms, f.state, 1<<20, f.s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	current, err := restarted.currentSave("snes/game.rom")
	if err != nil || current.Revision != r3.Revision {
		t.Fatalf("persistence: %v %v", current, err)
	}
}

func TestRejectedUploadsDoNotMutateSave(t *testing.T) {
	f := setup(t, 32)
	ctx, _ := f.enroll(t, pb.Profile_MIYOO, true)
	r, err := upload(t, f.client, ctx, []byte("original"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = upload(t, f.client, ctx, bytes.Repeat([]byte("x"), 33), r.Revision)
	requireCode(t, err, codes.ResourceExhausted)
	stream, err := f.client.UploadSave(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.FileChunk{GameId: "snes/game.rom", Sha256: digest("not the data")}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.FileChunk{Data: []byte("bad")}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.CloseAndRecv()
	requireCode(t, err, codes.DataLoss)
	current, err := f.s.currentSave("snes/game.rom")
	if err != nil || current.Revision != r.Revision {
		t.Fatalf("save mutated: %v %v", current, err)
	}
	files, err := os.ReadDir(filepath.Join(f.state, "backups"))
	if err != nil || len(files) != 1 {
		t.Fatalf("staging files leaked: %v %v", files, err)
	}
}

func TestSaveStateMatrix(t *testing.T) {
	a, b := digest("a"), digest("b")
	for _, tc := range []struct {
		current        saveRecord
		base, checksum string
		want           pb.SaveState
	}{
		{saveRecord{}, "", "", pb.SaveState_UP_TO_DATE},
		{saveRecord{}, "", a, pb.SaveState_CLIENT_IS_NEWER},
		{saveRecord{Revision: a, Checksum: a}, a, a, pb.SaveState_UP_TO_DATE},
		{saveRecord{Revision: a, Checksum: a}, a, b, pb.SaveState_CLIENT_IS_NEWER},
		{saveRecord{Revision: b, Checksum: b}, a, a, pb.SaveState_SERVER_IS_NEWER},
		{saveRecord{Revision: b, Checksum: b}, "", "", pb.SaveState_SERVER_IS_NEWER},
		{saveRecord{Revision: b, Checksum: b}, a, digest("c"), pb.SaveState_CONFLICT},
	} {
		if got := saveState(tc.current, tc.base, tc.checksum); got != tc.want {
			t.Errorf("got %s want %s for %+v", got, tc.want, tc)
		}
	}
}

func TestCatalogWatchUpdates(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_BATOCERA, true)
	watchCtx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- f.s.Watch(watchCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	stream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	// Repeated writes avoid depending on watcher startup scheduling.
	writeDone := make(chan struct{})
	writerStopped := make(chan struct{})
	go func() {
		defer close(writerStopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-writeDone:
				return
			case <-ticker.C:
				if err := os.WriteFile(filepath.Join(f.roms, "snes", "new.rom"), []byte("new"), 0644); err != nil {
					return
				}
				time.Sleep(400 * time.Millisecond)
			}
		}
	}()
	defer func() { close(writeDone); <-writerStopped }()
	next, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == next.Revision || len(next.Entries) != 3 {
		t.Fatalf("update missing: %v", next)
	}
}

func TestConcurrentUploadsPreserveEveryVersion(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_BATOCERA, true)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := upload(t, f.client, ctx, []byte{byte(i)}, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	files, err := os.ReadDir(filepath.Join(f.state, "backups"))
	if err != nil || len(files) != 8 {
		t.Fatalf("lost snapshots: %v %v", files, err)
	}
}

func TestProfileFiltering(t *testing.T) {
	catalog := &pb.Catalog{Entries: []*pb.CatalogEntry{
		{Id: "snes/game.rom", Platform: "snes", Kind: "rom"},
		{Id: "snes/gamelist.xml", Platform: "snes", Kind: "metadata"},
		{Id: "snes/miyoogamelist.xml", Platform: "snes", Kind: "metadata"},
		{Id: "snes/tvosgamelist.xml", Platform: "snes", Kind: "metadata"},
		{Id: "snes/media/batocera/a.png", Platform: "snes", Kind: "media"},
		{Id: "snes/media/miyoo/a.png", Platform: "snes", Kind: "media"},
		{Id: "snes/media/tvos/a.png", Platform: "snes", Kind: "media"},
	}}
	for _, profile := range []pb.Profile{pb.Profile_BATOCERA, pb.Profile_MIYOO, pb.Profile_TVOS} {
		got := filterCatalog(catalog, profile)
		if len(got.Entries) != 3 {
			t.Fatalf("%s: %v", profile, got)
		}
		if !strings.Contains(got.Entries[2].Id, strings.ToLower(profile.String())) {
			t.Fatalf("wrong profile: %v", got)
		}
	}
}

func TestInvalidChunkAndStaleDownloads(t *testing.T) {
	f := setup(t, 1<<20)
	ctx, _ := f.enroll(t, pb.Profile_MIYOO, true)
	stream, err := f.client.UploadSave(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.FileChunk{GameId: "snes/game.rom", Sha256: digest("x")}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.FileChunk{Data: bytes.Repeat([]byte("x"), ChunkSize+1)}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.CloseAndRecv()
	requireCode(t, err, codes.InvalidArgument)
	save, err := upload(t, f.client, ctx, []byte("save"), "")
	if err != nil {
		t.Fatal(err)
	}
	download, err := f.client.DownloadSave(ctx, &pb.DownloadSaveRequest{GameId: "snes/game.rom", Revision: digest("old")})
	if err == nil {
		_, err = download.Recv()
	}
	requireCode(t, err, codes.FailedPrecondition)
	if save.Revision == "" {
		t.Fatal("missing revision")
	}
	rom, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: "snes/game.rom", Sha256: digest("old")})
	if err == nil {
		_, err = rom.Recv()
	}
	requireCode(t, err, codes.FailedPrecondition)
}

func TestStateCannotBeInsideROMs(t *testing.T) {
	dir := t.TempDir()
	_, err := New(dir, filepath.Join(dir, "state"), 1024, slog.Default())
	if err == nil {
		t.Fatal("state exposed in ROM catalog")
	}
}

func TestRegistrationValidationAndHashedTokens(t *testing.T) {
	f := setup(t, 1024)
	for _, req := range []*pb.RegisterDeviceRequest{
		{Name: "", Profile: pb.Profile_MIYOO},
		{Name: "test", Profile: pb.Profile_PROFILE_UNSPECIFIED},
		{Name: strings.Repeat("x", 129), Profile: pb.Profile_BATOCERA},
	} {
		_, err := f.client.RegisterDevice(f.ctx, req)
		requireCode(t, err, codes.InvalidArgument)
	}
	result, err := f.client.RegisterDevice(f.ctx, &pb.RegisterDeviceRequest{Name: "device", Profile: pb.Profile_MIYOO})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.state, "devices", result.DeviceId+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(result.Token)) || !bytes.Contains(data, []byte(digest(result.Token))) {
		t.Fatal("raw token stored or token hash missing")
	}
}
func TestProfileImages(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".enrichment"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "game.rom"), []byte("rom"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(base, ".enrichment", "image.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 1600, 800))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	xmlData := `<gameList><game><path>./game.rom</path><name>Test Game</name><image>./.enrichment/image.png</image><desc>Test &amp; metadata</desc><rating>0.8</rating></game></gameList>`
	if err := os.WriteFile(filepath.Join(base, ".enrichment", "gamelist.xml"), []byte(xmlData), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir(".composite", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".composite", "gamelist.xml"), []byte(xmlData), 0644); err != nil {
		t.Fatal(err)
	}
	if err := buildProfiles(context.Background(), root, root, base, base, base, base, nil); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(base, "media", "miyoo", "composite"))
	if err != nil || len(files) != 1 {
		t.Fatalf("%v %v", files, err)
	}
	img, err := os.Open(filepath.Join(base, "media", "miyoo", "composite", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	cfg, err := png.DecodeConfig(img)
	if err != nil || cfg.Width != 640 || cfg.Height != 320 {
		t.Fatalf("wrong aspect ratio: %v %v", cfg, err)
	}
	data, err := os.ReadFile(filepath.Join(base, "miyoogamelist.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "./media/miyoo/") || !strings.Contains(string(data), "<desc>Test &amp; metadata</desc>") {
		t.Fatalf("metadata: %s", data)
	}
	for _, name := range []string{"../outside.png", filepath.Join(filepath.Dir(base), "outside.png")} {
		if _, err := relativeAsset(base, name); err == nil {
			t.Fatalf("accepted unsafe asset: %s", name)
		}
	}
}
