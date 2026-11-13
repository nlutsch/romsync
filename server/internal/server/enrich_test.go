package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("ROMSYNC_TEST_SCRAPER") == "1" {
		if err := fakeScraper(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeScraper(args []string) error {
	options := make(map[string]string)
	for i := 0; i+1 < len(args); i += 2 {
		options[args[i]] = args[i+1]
	}
	if platform := os.Getenv("ROMSYNC_TEST_SCRAPER_PLATFORM"); platform != "" && options["-p"] != platform {
		return fmt.Errorf("scraper received noncanonical platform %q", options["-p"])
	}
	if folder := os.Getenv("ROMSYNC_TEST_SCRAPER_FOLDER"); folder != "" && filepath.Base(options["-i"]) != folder {
		return fmt.Errorf("scraper did not receive physical ROM directory: %q", options["-i"])
	}
	if (options["-f"] != "emulationstation" && options["-f"] != "batocera" && options["-f"] != "pegasus") || !strings.HasPrefix(options["--flags"], "unattend,relative") {
		return fmt.Errorf("incorrect export options")
	}
	marker := filepath.Join(filepath.Dir(options["-g"]), "gathered")
	if options["-s"] != "" {
		return os.WriteFile(marker, []byte("cache gathered"), 0600)
	}
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("export ran before gather: %w", err)
	}
	img, err := os.Create(filepath.Join(options["-o"], "image.png"))
	if err != nil {
		return err
	}
	artwork := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			artwork.SetRGBA(x, y, color.RGBA{R: 80, G: 160, B: 220, A: 255})
		}
	}
	err = png.Encode(img, artwork)
	img.Close()
	if err != nil {
		return err
	}
	imagePath := filepath.Join(options["-o"], "image.png")
	if options["-f"] == "pegasus" {
		return os.WriteFile(filepath.Join(options["-g"], "metadata.pegasus.txt"),
			[]byte("game: Test\nfile: "+filepath.Join(options["-i"], "game.sfc")+"\nassets.wheel: "+imagePath+"\nassets.marquee: "+imagePath+"\n"), 0644)
	}
	g := game{Path: "./game.sfc", Name: "Test", Image: imagePath,
		Other: []element{{XMLName: xml.Name{Local: "desc"}, Value: "Explore a colorful world &amp; discover every secret."},
			{XMLName: xml.Name{Local: "developer"}, Value: "Test Studio"}}}
	if options["-f"] == "batocera" {
		g.Thumbnail, g.Fanart = imagePath, imagePath
		g.Video, g.Manual = filepath.Join(options["-o"], "video.mp4"), filepath.Join(options["-o"], "manual.pdf")
		for name, data := range map[string]string{g.Video: "synthetic video fixture", g.Manual: "synthetic manual fixture"} {
			if err := os.WriteFile(name, []byte(data), 0644); err != nil {
				return err
			}
		}
	}
	data, err := xml.Marshal(gameList{Games: []game{g}})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(options["-g"], "gamelist.xml"), data, 0600)
}

func TestEnrichRunsGatherThenExport(t *testing.T) {
	t.Setenv("ROMSYNC_TEST_SCRAPER", "1")
	t.Setenv("ROMSYNC_TEST_SCRAPER_PLATFORM", "snes")
	t.Setenv("ROMSYNC_TEST_SCRAPER_FOLDER", "SFC")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "SFC"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SFC", "game.sfc"), []byte("ROM"), 0644); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Enrich(context.Background(), root, "SNES", ScraperOptions{Binary: binary, Source: "screenscraper"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	// A second export exercises atomic replacement of existing manifests and assets.
	if err := Enrich(context.Background(), root, "sfc", ScraperOptions{Binary: binary, Source: "screenscraper"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gamelist.xml", "miyoogamelist.xml", "tvosgamelist.xml"} {
		f, err := os.Open(filepath.Join(root, generatedDir, "snes", name))
		if err != nil {
			t.Fatal(err)
		}
		var list gameList
		err = xml.NewDecoder(f).Decode(&list)
		f.Close()
		if err != nil || len(list.Games) != 1 {
			t.Fatalf("%s: %v %v", name, list, err)
		}
		img, err := os.Open(filepath.Join(root, generatedDir, "snes", filepath.FromSlash(list.Games[0].Image)))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(img)
		img.Close()
		if err != nil || cfg.Width != 100 || cfg.Height != 50 {
			t.Fatalf("small image upscaled: %v %v", cfg, err)
		}
	}
}

func TestEnrichPublishesStagedProgress(t *testing.T) {
	t.Setenv("ROMSYNC_TEST_SCRAPER", "1")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "snes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "snes", "game.sfc"), []byte("ROM"), 0644); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	publish := func(g game) {
		count++
		if g.Name != "Test" || g.Path != "./game.sfc" || len(g.Other) == 0 {
			t.Fatalf("incomplete metadata preview: %+v", g)
		}
		if _, err := os.Stat(filepath.Join(root, generatedDir, "snes", "gamelist.xml")); !os.IsNotExist(err) {
			t.Fatal("progress waited until the platform manifest was complete")
		}
		switch count {
		case 1:
			if g.Image != "" || g.Video != "" || g.Manual != "" {
				t.Fatal("text preview exposed raw scraper asset paths")
			}
			if _, err := os.Stat(filepath.Join(root, generatedDir, "snes", ".logos", "metadata.pegasus.txt")); !os.IsNotExist(err) {
				t.Fatal("text preview waited for later export passes")
			}
		case 2:
			if !strings.HasPrefix(g.Image, "./media/batocera/") {
				t.Fatalf("profile preview contains invalid artwork: %q", g.Image)
			}
			if _, err := os.Stat(filepath.Join(root, generatedDir, "snes", filepath.FromSlash(g.Image))); err != nil {
				t.Fatal("game published before its artwork was available")
			}
		default:
			t.Fatal("non-dashboard profile unexpectedly published a preview")
		}
	}
	if err := Enrich(context.Background(), root, "snes", ScraperOptions{Binary: binary, Source: "screenscraper", progress: publish}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected text and completed-game updates, got %d", count)
	}
}

func TestEnrichPropagatesFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "snes"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Enrich(context.Background(), root, "../outside", ScraperOptions{Binary: "missing", Source: "screenscraper"}, io.Discard); err == nil {
		t.Fatal("unsafe platform accepted")
	}
	if err := Enrich(context.Background(), root, "snes", ScraperOptions{Binary: filepath.Join(root, "missing"), Source: "screenscraper"}, io.Discard); err == nil {
		t.Fatal("missing scraper ignored")
	}
}

func TestRelativeScraperAsset(t *testing.T) {
	base := t.TempDir()
	exportRoot := "/mnt/c/ROM collection/snes"
	for _, name := range []string{"./game.sfc", exportRoot + "/game.sfc"} {
		got, err := relativeScraperAsset(base, exportRoot, name)
		if err != nil || got != "game.sfc" {
			t.Fatalf("%s: %s %v", name, got, err)
		}
	}
	for _, name := range []string{"../outside.png", "/etc/passwd", exportRoot + "-other/game.sfc", exportRoot + "/../outside.png"} {
		if _, err := relativeScraperAsset(base, exportRoot, name); err == nil {
			t.Fatalf("unsafe export accepted: %s", name)
		}
	}
}

func TestWSLEnrichment(t *testing.T) {
	distribution := os.Getenv("ROMSYNC_TEST_WSL")
	if distribution == "" {
		t.Skip("set ROMSYNC_TEST_WSL to a configured WSL distribution for an offline integration test")
	}
	if runtime.GOOS != "windows" {
		t.Skip("Windows-to-WSL integration test")
	}
	base := filepath.Join(t.TempDir(), "ROM collection")
	platform := filepath.Join(base, "snes")
	dataRoot := filepath.Join(base, generatedDir, "snes")
	if err := os.MkdirAll(filepath.Join(dataRoot, ".enrichment"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(platform, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(platform, "test.sfc"), []byte("synthetic test data, not a game: "+base), 0644); err != nil {
		t.Fatal(err)
	}
	art, err := os.Create(filepath.Join(dataRoot, ".enrichment", "source.png"))
	if err != nil {
		t.Fatal(err)
	}
	artwork := image.NewRGBA(image.Rect(0, 0, 100, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 100; x++ {
			artwork.SetRGBA(x, y, color.RGBA{R: 80, G: 160, B: 220, A: 255})
		}
	}
	encodeErr := png.Encode(art, artwork)
	closeErr := art.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatalf("fixture artwork: %v %v", encodeErr, closeErr)
	}
	sources := map[string]string{"source.mp4": strings.Repeat("synthetic video fixture", 256), "source.pdf": "%PDF-1.4 synthetic manual fixture"}
	for name, data := range sources {
		if err := os.WriteFile(filepath.Join(dataRoot, ".enrichment", name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	sourceImage := "../romsync-data/snes/.enrichment/source.png"
	data := []byte(`<gameList><game><path>./test.sfc</path><name>Offline test</name><desc>Offline import fixture</desc><image>` + sourceImage + `</image><thumbnail>` + sourceImage + `</thumbnail><marquee>` + sourceImage + `</marquee><fanart>` + sourceImage + `</fanart><video>../romsync-data/snes/.enrichment/source.mp4</video><manual>../romsync-data/snes/.enrichment/source.pdf</manual></game></gameList>`)
	for _, name := range []string{filepath.Join(".enrichment", "gamelist.xml")} {
		if err := os.WriteFile(filepath.Join(dataRoot, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var output strings.Builder
	err = Enrich(ctx, base, "snes", ScraperOptions{Binary: "Skyscraper", Source: "esgamelist", WSLDistribution: distribution}, &output)
	if err != nil {
		export, _ := os.ReadFile(filepath.Join(dataRoot, ".enrichment", "gamelist.xml"))
		t.Fatalf("WSL enrichment failed: %v\nExport: %s\n%s", err, export, output.String())
	}
	for _, name := range []string{"gamelist.xml", "miyoogamelist.xml", "tvosgamelist.xml"} {
		f, err := os.Open(filepath.Join(dataRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		var list gameList
		err = xml.NewDecoder(f).Decode(&list)
		f.Close()
		if err != nil || len(list.Games) != 1 || list.Games[0].Name != "Offline test" {
			t.Fatalf("unexpected %s output: %+v %v\n%s", name, list, err, output.String())
		}
		g := list.Games[0]
		if g.Image == "" || g.Screenshot == "" || g.Thumbnail == "" || g.Wheel == "" || g.Fanart == "" || g.Video == "" || g.Manual == "" {
			raw, _ := os.ReadFile(filepath.Join(dataRoot, ".enrichment", "gamelist.xml"))
			t.Fatalf("missing independent artwork in %s: %+v\nRaw: %s\n%s", name, g, raw, output.String())
		}
		img, err := os.Open(filepath.Join(dataRoot, filepath.FromSlash(list.Games[0].Image)))
		if err != nil {
			t.Fatal(err)
		}
		config, err := png.DecodeConfig(img)
		img.Close()
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			t.Fatalf("invalid exported PNG: %+v %v", config, err)
		}
		for filename, source := range map[string]string{g.Video: sources["source.mp4"], g.Manual: sources["source.pdf"]} {
			content, err := os.ReadFile(filepath.Join(dataRoot, filepath.FromSlash(filename)))
			if err != nil || string(content) != source {
				t.Fatalf("media payload changed: %s %v", filename, err)
			}
		}
	}
	files, err := os.ReadDir(platform)
	if err != nil || len(files) != 1 || files[0].Name() != "test.sfc" {
		t.Fatalf("scraping polluted ROM folder: %v %v", files, err)
	}
}
func TestPortraitImageFitsMiyooBounds(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "portrait.png"))
	if err != nil {
		t.Fatal(err)
	}
	err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 800, 1600)))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := resizePNG(root, "portrait.png", "out.png", 640, 480); err != nil {
		t.Fatal(err)
	}
	out, err := root.Open("out.png")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cfg, err := png.DecodeConfig(out)
	if err != nil || cfg.Width != 240 || cfg.Height != 480 {
		t.Fatalf("portrait bounds: %v %v", cfg, err)
	}
}
