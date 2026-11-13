package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc/codes"
)

func TestCatalogCacheStartupAndBackgroundReconciliation(t *testing.T) {
	f := setup(t, 1024)
	if err := os.Rename(filepath.Join(f.roms, "snes"), filepath.Join(f.roms, "SFC")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.roms, "SFC", "game.rom"), []byte("modified ROM"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.roms, "SFC", "new.sfc"), []byte("new ROM"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.roms, "ps2", "game.rom")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	app, err := NewCachedContext(f.ctx, f.roms, f.state, 1024, f.s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cached startup took %v", elapsed)
	}
	if len(app.catalog.Entries) != 2 || app.catalog.Entries[1].Sha256 != digest("ROM contents") {
		t.Fatal("startup did not immediately serve the saved snapshot")
	}
	changed := app.changed
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() { done <- app.Watch(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("background reconciliation did not publish the changes")
	}
	app.catalogMu.RLock()
	catalog := app.catalog
	app.catalogMu.RUnlock()
	if len(catalog.Entries) != 2 {
		t.Fatalf("addition/deletion not reconciled: %v", catalog)
	}
	for _, entry := range catalog.Entries {
		if entry.Platform != "snes" || app.physicalPath(entry) != "SFC/"+entry.TargetPath {
			t.Fatalf("canonical/physical paths incorrect: %v", entry)
		}
		if entry.Id == "snes/game.rom" && entry.Sha256 != digest("modified ROM") {
			t.Fatal("modified ROM retained its old checksum")
		}
	}
	restarted, err := NewCachedContext(f.ctx, f.roms, f.state, 1024, f.s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.catalog.Revision != catalog.Revision || restarted.physicalPath(restarted.catalog.Entries[0]) != "SFC/game.rom" {
		t.Fatal("reconciled catalog and physical paths did not survive restart")
	}
}

func TestInvalidCatalogCacheIsReportedAndRebuilt(t *testing.T) {
	for _, name := range []string{"truncated", "root", "path", "checksum"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, 1024)
			cache := f.s.cache
			switch name {
			case "root":
				cache.ROMRoot = filepath.Join(t.TempDir(), "other-library")
			case "path":
				cache.Paths["snes/game.rom"] = "../private"
			case "checksum":
				file := cache.Files["snes/game.rom"]
				file.SHA256 = digest("incorrect")
				cache.Files["snes/game.rom"] = file
			}
			data, err := json.Marshal(cache)
			if err != nil {
				t.Fatal(err)
			}
			if name == "truncated" {
				data = []byte("{")
			}
			if err := os.WriteFile(filepath.Join(f.state, "catalog.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			app, err := NewCachedContext(f.ctx, f.roms, f.state, 1024, slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			if len(app.catalog.Entries) != 0 || !strings.Contains(logs.String(), "rebuilding") {
				t.Fatalf("invalid cache used or silently ignored: %v %s", app.catalog, logs.String())
			}
			if err := app.Scan(); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewCachedContext(f.ctx, f.roms, f.state, 1024, f.s.log)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if len(restarted.catalog.Entries) != 2 {
				t.Fatal("rebuilt cache was not persisted")
			}
		})
	}
}

func TestPlatformAliasesPreserveDownloads(t *testing.T) {
	for _, folder := range []string{"snes", "sfc", "SNES", "SFC", "Super Nintendo"} {
		t.Run(folder, func(t *testing.T) {
			f := setup(t, 1024)
			if folder != "snes" {
				if err := os.Rename(filepath.Join(f.roms, "snes"), filepath.Join(f.roms, folder)); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.Scan(); err != nil {
				t.Fatal(err)
			}
			ctx, _ := f.enroll(t, pb.Profile_TVOS, true)
			stream, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: "snes/game.rom", Sha256: digest("ROM contents")})
			if err != nil {
				t.Fatal(err)
			}
			var downloaded []byte
			for {
				chunk, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				downloaded = append(downloaded, chunk.Data...)
			}
			if string(downloaded) != "ROM contents" {
				t.Fatalf("incorrect download: %q", downloaded)
			}
		})
	}
}

func TestDuplicatePlatformAliasesAreRejected(t *testing.T) {
	f := setup(t, 1024)
	revision := f.s.catalog.Revision
	if err := os.Mkdir(filepath.Join(f.roms, "SFC"), 0755); err != nil {
		t.Fatal(err)
	}
	err := f.s.Scan()
	if err == nil || !strings.Contains(err.Error(), `"SFC"`) || !strings.Contains(err.Error(), `"snes"`) {
		t.Fatalf("duplicate alias not reported: %v", err)
	}
	if f.s.catalog.Revision != revision {
		t.Fatal("failed scan replaced the usable catalog")
	}
}

func TestCatalogPersistenceFailurePreservesSnapshot(t *testing.T) {
	f := setup(t, 1024)
	revision := f.s.catalog.Revision
	filename := filepath.Join(f.state, "catalog.json")
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filename, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.roms, "snes", "new.sfc"), []byte("new ROM"), 0644); err != nil {
		t.Fatal(err)
	}
	err := f.s.Scan()
	if err == nil || !strings.Contains(err.Error(), "persist catalog") {
		t.Fatalf("catalog persistence failure not reported: %v", err)
	}
	if f.s.catalog.Revision != revision || f.s.cache.Catalog.Revision != revision || f.s.scanError == "" {
		t.Fatal("storage failure replaced the usable snapshot or was not surfaced")
	}
}

func TestColdScanPublishesEachSystemBeforeCompletion(t *testing.T) {
	base := t.TempDir()
	roms := filepath.Join(base, "roms")
	for _, folder := range []string{"FC", "SFC"} {
		if err := os.MkdirAll(filepath.Join(roms, folder), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(roms, folder, "game.rom"), []byte("ROM"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	app, err := NewCachedContext(context.Background(), roms, filepath.Join(base, "state"), 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	count := 0
	app.onPlatformScanned = func(platform, revision, completed string) {
		count++
		if len(app.catalog.Entries) != count || app.catalog.Entries[count-1].Platform != platform {
			t.Fatal("system scan notification preceded publication or waited for the whole library")
		}
		var persisted catalogCache
		data, err := os.ReadFile(filepath.Join(base, "state", "catalog.json"))
		if err != nil || json.Unmarshal(data, &persisted) != nil || len(persisted.Catalog.Entries) != count {
			t.Fatal("completed system was not persisted before notification")
		}
	}
	if err := app.Scan(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("system completion notifications: %d", count)
	}
}

func TestCachedChecksumsStillVerifyDownloads(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_BATOCERA, true)
	filename := filepath.Join(f.roms, "snes", "game.rom")
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("BAD contents"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filename, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Scan(); err != nil {
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

func TestCanceledStartupScan(t *testing.T) {
	dir := t.TempDir()
	roms := filepath.Join(dir, "roms")
	if err := os.Mkdir(roms, 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app, err := NewContext(ctx, roms, filepath.Join(dir, "state"), 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if app != nil {
		app.Close()
		t.Fatal("canceled scan returned an initialized server")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected scan cancellation, got %v", err)
	}
}

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
