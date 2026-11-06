package server

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc/codes"
)

func TestChangedROMAbortsDownload(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_BATOCERA, true)
	if err := os.WriteFile(filepath.Join(f.roms, "snes", "game.rom"), []byte("changed after catalog scan"), 0644); err != nil {
		t.Fatal(err)
	}
	stream, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: "snes/game.rom", Sha256: digest("ROM contents")})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if err != nil {
			break
		}
	}
	requireCode(t, err, codes.Aborted)
}

func TestHiddenFilesAndSymlinksAreNotPublished(t *testing.T) {
	f := setup(t, 1024)
	hidden := filepath.Join(f.roms, "snes", ".enrichment")
	if err := os.Mkdir(hidden, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "source.png"), []byte("private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.state, "devices"), filepath.Join(f.roms, "snes", "linked")); err != nil {
		t.Logf("symlink creation unavailable: %v", err)
	}
	if err := f.s.Scan(); err != nil {
		t.Fatal(err)
	}
	if len(f.s.catalog.Entries) != 2 {
		t.Fatalf("unexpected files exposed: %v", f.s.catalog)
	}
}

func TestEmptySaveRoundTrip(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_MIYOO, true)
	save, err := upload(t, f.client, ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := f.client.DownloadSave(ctx, &pb.DownloadSaveRequest{GameId: "snes/game.rom", Revision: save.Revision})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err != io.EOF {
		t.Fatalf("empty save: %v", err)
	}
}
