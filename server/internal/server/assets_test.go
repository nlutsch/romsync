package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

func seedAssets(t *testing.T, f *fixture) {
	t.Helper()
	base := filepath.Join(f.roms, generatedDir, "snes")
	if err := os.MkdirAll(filepath.Join(base, ".enrichment"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, ".composite"), 0755); err != nil {
		t.Fatal(err)
	}
	img, err := os.Create(filepath.Join(base, ".enrichment", "image.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(img, image.NewRGBA(image.Rect(0, 0, 100, 50))); err != nil {
		t.Fatal(err)
	}
	img.Close()
	for name, data := range map[string]string{"video.mp4": "synthetic video fixture", "manual.pdf": "synthetic manual fixture"} {
		if err := os.WriteFile(filepath.Join(base, ".enrichment", name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	g := game{Path: "./game.rom", Name: "Beautiful game", Image: "../romsync-data/snes/.enrichment/image.png", Thumbnail: "../romsync-data/snes/.enrichment/image.png",
		Wheel: "../romsync-data/snes/.enrichment/image.png", Marquee: "../romsync-data/snes/.enrichment/image.png", Fanart: "../romsync-data/snes/.enrichment/image.png",
		Video: "../romsync-data/snes/.enrichment/video.mp4", Manual: "../romsync-data/snes/.enrichment/manual.pdf",
		Other: []element{{XMLName: xml.Name{Local: "desc"}, Value: "A test &amp; a description"}, {XMLName: xml.Name{Local: "developer"}, Value: "Test Studio"}},
	}
	data, err := xml.Marshal(gameList{Games: []game{g}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".enrichment", ".composite"} {
		if err := os.WriteFile(filepath.Join(base, name, "gamelist.xml"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	romRoot, err := os.OpenRoot(filepath.Join(f.roms, "snes"))
	if err != nil {
		t.Fatal(err)
	}
	defer romRoot.Close()
	romAbs := filepath.Join(f.roms, "snes")
	if err := buildProfiles(context.Background(), root, romRoot, romAbs, romAbs, base, base); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Scan(); err != nil {
		t.Fatal(err)
	}
}

func receiveFile(t *testing.T, f *fixture, ctx context.Context, entry *pb.CatalogEntry, selection *pb.AssetSelection) []byte {
	t.Helper()
	stream, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: entry.Id, Sha256: entry.Sha256, Assets: selection})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for {
		chunk, err := stream.Recv()
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
	hash := sha256.Sum256(out.Bytes())
	if hex.EncodeToString(hash[:]) != entry.Sha256 {
		t.Fatal("catalog checksum differs from actual bytes")
	}
	return out.Bytes()
}

func TestAssetSelectionsAndPersistence(t *testing.T) {
	f := setup(t, 1024)
	seedAssets(t, f)
	ctx, id := f.enroll(t, pb.Profile_BATOCERA, true)
	selection := &pb.AssetSelection{Types: []pb.AssetType{pb.AssetType_BOXART, pb.AssetType_VIDEO, pb.AssetType_MANUAL}}
	if _, err := f.client.SetAssetPreferences(ctx, &pb.SetAssetPreferencesRequest{Assets: selection}); err != nil {
		t.Fatal(err)
	}
	var d Device
	if err := readJSON(f.s.state, filepath.Join("devices", id+".json"), &d); err != nil {
		t.Fatal(err)
	}
	if d.Assets == nil || len(d.Assets.Types) != 3 {
		t.Fatal("preferences not persisted")
	}
	stream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[pb.AssetType]bool{}
	for _, entry := range catalog.Entries {
		if entry.Kind == "media" {
			seen[entry.AssetType] = true
			if entry.GameId != "snes/game.rom" || entry.TargetPath == "" {
				t.Fatalf("unassociated asset: %v", entry)
			}
			if !wants(selection, entry.AssetType) {
				t.Fatalf("unrequested asset: %v", entry)
			}
			receiveFile(t, f, ctx, entry, nil)
		}
		if entry.Kind == "metadata" {
			data := receiveFile(t, f, ctx, entry, nil)
			var list gameList
			if err := xml.Unmarshal(data, &list); err != nil {
				t.Fatal(err)
			}
			g := list.Games[0]
			if g.Image != "" || g.Thumbnail == "" || g.Video == "" || g.Manual == "" || g.Wheel != "" {
				t.Fatalf("bad selected manifest: %+v", g)
			}
			if bytes.Contains(data, []byte("<screenshot>")) {
				t.Fatal("unrequested screenshot reference")
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("missing asset types: %v", seen)
	}
	empty := &pb.AssetSelection{}
	emptyStream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{Assets: empty})
	if err != nil {
		t.Fatal(err)
	}
	noMedia, err := emptyStream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range noMedia.Entries {
		if entry.Kind == "media" {
			t.Fatal("empty override returned media")
		}
		if entry.Kind == "metadata" {
			receiveFile(t, f, ctx, entry, empty)
		}
	}
	all := &pb.AssetSelection{}
	for kind := range assetNames {
		all.Types = append(all.Types, kind)
	}
	allCatalog, _, err := f.s.deviceCatalog(f.s.catalog, pb.Profile_BATOCERA, all)
	if err != nil {
		t.Fatal(err)
	}
	media := 0
	for _, entry := range allCatalog.Entries {
		if entry.Kind == "media" {
			media++
			receiveFile(t, f, ctx, entry, all)
		}
	}
	if media != 8 {
		t.Fatalf("want 8 independent asset types, got %d", media)
	}
	screenshots := &pb.AssetSelection{Types: []pb.AssetType{pb.AssetType_SCREENSHOT}}
	screenshotCatalog, _, err := f.s.deviceCatalog(f.s.catalog, pb.Profile_BATOCERA, screenshots)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range screenshotCatalog.Entries {
		if entry.Kind == "metadata" {
			data := receiveFile(t, f, ctx, entry, screenshots)
			var list gameList
			if err := xml.Unmarshal(data, &list); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(list.Games[0].Image, "/screenshot/") || list.Games[0].Screenshot != "" {
				t.Fatal("screenshot-only selection must use standard image tag")
			}
		}
	}
	for _, entry := range allCatalog.Entries {
		if entry.AssetType == pb.AssetType_COMPOSITE {
			download, err := f.client.DownloadFile(ctx, &pb.DownloadFileRequest{Id: entry.Id, Sha256: entry.Sha256})
			if err != nil {
				t.Fatal(err)
			}
			_, err = download.Recv()
			requireCode(t, err, codes.NotFound)
		}
	}
	restarted, err := New(f.roms, f.state, 1024, f.s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	incoming := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", extractAuthorization(ctx)))
	reloaded, err := restarted.authenticate(incoming)
	if err != nil {
		t.Fatal(err)
	}
	if len(selectionFor(reloaded, nil).Types) != 3 {
		t.Fatal("preferences lost after restart")
	}
}

func extractAuthorization(ctx context.Context) string {
	md, _ := metadata.FromOutgoingContext(ctx)
	return md.Get("authorization")[0]
}

func TestAssetValidationAndPreferenceNotification(t *testing.T) {
	f := setup(t, 1024)
	ctx, _ := f.enroll(t, pb.Profile_MIYOO, true)
	for _, selection := range []*pb.AssetSelection{
		{Types: []pb.AssetType{pb.AssetType_ASSET_UNSPECIFIED}},
		{Types: []pb.AssetType{pb.AssetType(999)}},
		{Types: []pb.AssetType{pb.AssetType_BOXART, pb.AssetType_BOXART}},
	} {
		_, err := f.client.SetAssetPreferences(ctx, &pb.SetAssetPreferencesRequest{Assets: selection})
		requireCode(t, err, codes.InvalidArgument)
		_, err = f.client.RegisterDevice(f.ctx, &pb.RegisterDeviceRequest{Name: "invalid assets", Profile: pb.Profile_MIYOO, Assets: selection})
		requireCode(t, err, codes.InvalidArgument)
	}
	_, err := f.client.SetAssetPreferences(ctx, &pb.SetAssetPreferencesRequest{})
	requireCode(t, err, codes.InvalidArgument)
	_, err = f.client.SetAssetPreferences(f.ctx, &pb.SetAssetPreferencesRequest{Assets: &pb.AssetSelection{}})
	requireCode(t, err, codes.Unauthenticated)
	stream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.SetAssetPreferences(ctx, &pb.SetAssetPreferencesRequest{Assets: &pb.AssetSelection{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal("preference change did not notify watcher:", err)
	}
}

func TestManifestReferencesOnlyAvailableAssets(t *testing.T) {
	f := setup(t, 1024)
	seedAssets(t, f)
	for _, entry := range f.s.catalog.Entries {
		if strings.Contains(entry.Id, "/batocera/") && (entry.AssetType == pb.AssetType_COMPOSITE || entry.AssetType == pb.AssetType_MANUAL) {
			if err := os.Remove(filepath.Join(f.roms, catalogPhysicalPath(entry))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.s.Scan(); err != nil {
		t.Fatal(err)
	}
	selection := &pb.AssetSelection{}
	for kind := range assetNames {
		selection.Types = append(selection.Types, kind)
	}
	catalog, manifests, err := f.s.deviceCatalog(f.s.catalog, pb.Profile_BATOCERA, selection)
	if err != nil {
		t.Fatal(err)
	}
	media := map[string]bool{}
	for _, entry := range catalog.Entries {
		if entry.Kind == "media" {
			media[entry.TargetPath] = true
		}
	}
	for _, data := range manifests {
		var list gameList
		if err := xml.Unmarshal(data, &list); err != nil {
			t.Fatal(err)
		}
		g := list.Games[0]
		if g.Manual != "" || !strings.Contains(g.Image, "/screenshot/") {
			t.Fatalf("unavailable media referenced: %+v", g)
		}
		for _, reference := range []string{g.Image, g.Screenshot, g.Thumbnail, g.Wheel, g.Marquee, g.Fanart, g.Video, g.Manual} {
			if reference != "" && !media[strings.TrimPrefix(reference, "./")] {
				t.Fatalf("reference outside delivered catalog: %s", reference)
			}
		}
	}
}

func TestRegistrationAssetPreferences(t *testing.T) {
	f := setup(t, 1024)
	seedAssets(t, f)
	for _, selection := range []*pb.AssetSelection{nil, {}, {Types: []pb.AssetType{pb.AssetType_BOXART}}} {
		reply, err := f.client.RegisterDevice(f.ctx, &pb.RegisterDeviceRequest{Name: "client assets", Profile: pb.Profile_MIYOO, Assets: selection})
		if err != nil {
			t.Fatal(err)
		}
		if err := Approve(f.s.state, reply.DeviceId, true); err != nil {
			t.Fatal(err)
		}
		ctx := metadata.AppendToOutgoingContext(f.ctx, "authorization", "Bearer "+reply.Token)
		stream, err := f.client.WatchCatalog(ctx, &pb.WatchCatalogRequest{})
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		expected := pb.AssetType_COMPOSITE
		if selection != nil {
			if len(selection.Types) == 0 {
				expected = pb.AssetType_ASSET_UNSPECIFIED
			} else {
				expected = pb.AssetType_BOXART
			}
		}
		count := 0
		for _, entry := range catalog.Entries {
			if entry.Kind == "media" {
				count++
				if entry.AssetType != expected || !strings.Contains(entry.Id, "/miyoo/") {
					t.Fatalf("wrong registered selection: %v", entry)
				}
			}
		}
		if (expected == pb.AssetType_ASSET_UNSPECIFIED && count != 0) || (expected != pb.AssetType_ASSET_UNSPECIFIED && count != 1) {
			t.Fatalf("unexpected media count: %d", count)
		}
	}
}

func TestGeneratedTreeNeverBecomesROMs(t *testing.T) {
	f := setup(t, 1024)
	seedAssets(t, f)
	if err := os.WriteFile(filepath.Join(f.roms, generatedDir, "snes", "secret.rom"), []byte("not a ROM"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := f.s.Scan(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range f.s.catalog.Entries {
		if entry.Platform == generatedDir || strings.Contains(entry.Id, ".enrichment") || strings.Contains(entry.Id, "secret.rom") {
			t.Fatalf("private export exposed: %v", entry)
		}
	}
	files, err := os.ReadDir(filepath.Join(f.roms, "snes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "game.rom" {
		t.Fatalf("ROM directory polluted: %v", files)
	}
	a := setupAdmin(t, f, 1024)
	response := adminRequest(t, a, "GET", "/api/library", "")
	view := decodeResponse[struct {
		Games     []adminGame `json:"games"`
		Platforms []string    `json:"platforms"`
	}](t, response, 200)
	for _, p := range view.Platforms {
		if p == generatedDir {
			t.Fatal("generated folder offered as platform")
		}
	}
	for _, g := range view.Games {
		if g.ID == "snes/game.rom" && (g.Metadata["desc"] != "A test & a description" || len(g.Assets) != 8) {
			t.Fatalf("missing web metadata: %+v", g)
		}
	}
}
