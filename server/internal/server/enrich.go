package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/image/draw"
)

type gameList struct {
	XMLName xml.Name `xml:"gameList"`
	Games   []game   `xml:"game"`
}

type element struct {
	XMLName xml.Name
	Value   string `xml:",innerxml"`
}

type game struct {
	Path       string    `xml:"path"`
	Name       string    `xml:"name"`
	Image      string    `xml:"image,omitempty"`
	Screenshot string    `xml:"screenshot,omitempty"`
	Thumbnail  string    `xml:"thumbnail,omitempty"`
	Video      string    `xml:"video,omitempty"`
	Manual     string    `xml:"manual,omitempty"`
	Fanart     string    `xml:"fanart,omitempty"`
	Wheel      string    `xml:"wheel,omitempty"`
	Marquee    string    `xml:"marquee,omitempty"`
	Other      []element `xml:",any"`
}

type ScraperOptions struct {
	Binary          string
	Source          string
	WSLDistribution string
	progress        func(game)
}

func wslOutput(ctx context.Context, distribution string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "wsl.exe", append([]string{"--distribution", distribution, "--exec"}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("WSL command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	result := strings.TrimSpace(string(output))
	if result == "" || strings.ContainsAny(result, "\r\n") {
		return "", fmt.Errorf("WSL command returned an invalid empty or multiline path")
	}
	return result, nil
}

// Enrich gathers metadata, exports an EmulationStation game list, and builds profile assets.
// Credentials and scraper-specific settings stay in Skyscraper's own configuration.
func Enrich(ctx context.Context, romPath, platform string, options ScraperOptions, output io.Writer) error {
	root, err := os.OpenRoot(romPath)
	if err != nil {
		return err
	}
	defer root.Close()
	platform, physical, err := resolvePlatform(root, platform)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Join(generatedDir, platform, ".enrichment", "media"), 0755); err != nil {
		return err
	}
	dir, err := root.OpenRoot(filepath.Join(generatedDir, platform))
	if err != nil {
		return err
	}
	defer dir.Close()
	romDir, err := root.OpenRoot(physical)
	if err != nil {
		return err
	}
	defer romDir.Close()
	if err := dir.MkdirAll(".enrichment/media", 0700); err != nil {
		return err
	}
	abs, err := filepath.Abs(filepath.Join(romPath, physical))
	if err != nil {
		return err
	}
	dataAbs, err := filepath.Abs(filepath.Join(romPath, generatedDir, platform))
	if err != nil {
		return err
	}
	scrapeRoot, scrapeData := abs, dataAbs
	exportDir, mediaDir := filepath.Join(dataAbs, ".enrichment"), filepath.Join(dataAbs, ".enrichment", "media")
	binary := options.Binary
	var prefix []string
	if options.WSLDistribution != "" {
		if runtime.GOOS != "windows" {
			return fmt.Errorf("skyscraper-wsl is supported only by a Windows romsync server")
		}
		scrapeRoot, err = wslOutput(ctx, options.WSLDistribution, "wslpath", "-a", "-u", abs)
		if err != nil {
			return err
		}
		if !path.IsAbs(scrapeRoot) {
			return fmt.Errorf("WSL returned a non-absolute ROM path")
		}
		scrapeData, err = wslOutput(ctx, options.WSLDistribution, "wslpath", "-a", "-u", dataAbs)
		if err != nil {
			return err
		}
		exportDir, mediaDir = path.Join(scrapeData, ".enrichment"), path.Join(scrapeData, ".enrichment", "media")
		if binary == "Skyscraper" {
			home, err := wslOutput(ctx, options.WSLDistribution, "printenv", "HOME")
			if err != nil {
				return err
			}
			binary = path.Join(home, ".local", "bin", "Skyscraper")
		}
		prefix = []string{"--distribution", options.WSLDistribution, "--exec", binary}
		binary = "wsl.exe"
	}
	rawArtwork := `<artwork><output type="screenshot"/><output type="cover"/></artwork>`
	if err := atomicBytes(dir, filepath.Join(".enrichment", "raw-artwork.xml"), []byte(rawArtwork)); err != nil {
		return err
	}
	join := filepath.Join
	if options.WSLDistribution != "" {
		join = path.Join
	}
	common := []string{"-p", platform, "-i", scrapeRoot, "-f", "emulationstation", "-g", exportDir, "-o", mediaDir, "--flags", "unattend,relative,videos,manuals,fanarts"}
	raw := []string{"-p", platform, "-i", scrapeRoot, "-f", "batocera", "-g", exportDir, "-o", mediaDir,
		"--flags", "unattend,relative,videos,manuals,fanarts", "-a", join(exportDir, "raw-artwork.xml")}
	compositeDir := join(scrapeData, ".composite")
	if err := dir.MkdirAll(".composite/media", 0755); err != nil {
		return err
	}
	if err := dir.MkdirAll(".logos/media", 0755); err != nil {
		return err
	}
	if err := atomicBytes(dir, filepath.Join(".logos", "artwork.xml"), []byte(`<artwork><output type="wheel"/><output type="marquee"/></artwork>`)); err != nil {
		return err
	}
	logoDir := join(scrapeData, ".logos")
	logos := []string{"-p", platform, "-i", scrapeRoot, "-f", "pegasus", "-g", logoDir, "-o", join(logoDir, "media"),
		"--flags", "unattend,relative", "-a", join(logoDir, "artwork.xml")}
	composite := []string{"-p", platform, "-i", scrapeRoot, "-f", "emulationstation", "-g", compositeDir, "-o", join(compositeDir, "media"), "--flags", "unattend,relative"}
	for index, args := range [][]string{append(append([]string{}, common...), "-s", options.Source), raw, logos, composite} {
		stage := "Gathering metadata"
		if index == 1 {
			stage = "Exporting individual artwork, videos and manuals"
		}
		if index == 2 {
			stage = "Exporting separate wheel and marquee artwork"
		}
		if index == 3 {
			stage = "Exporting web composite artwork"
		}
		if _, err := fmt.Fprintf(output, "%s for %s...\n", stage, platform); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, binary, append(append([]string{}, prefix...), args...)...)
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("Skyscraper failed: %w", err)
		}
		if index == 1 && options.progress != nil {
			if err := previewMetadata(dir, romDir, abs, scrapeRoot, options.progress); err != nil {
				return err
			}
		}
	}
	if err := mergeLogos(dir, abs, scrapeRoot, dataAbs, scrapeData); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Generating Batocera, Miyoo, and tvOS profiles..."); err != nil {
		return err
	}
	if err := buildProfiles(ctx, dir, romDir, abs, scrapeRoot, dataAbs, scrapeData, options.progress); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "Metadata and artwork ready.")
	return err
}

func textualMetadata(elements []element) []element {
	var text []element
	for _, e := range elements {
		switch e.XMLName.Local {
		case "video", "marquee", "fanart", "wheel", "boxart", "texture", "boxback", "screenshot":
			continue
		}
		text = append(text, e)
	}
	return text
}

func previewMetadata(dir, romDir *os.Root, abs, scrapeRoot string, publish func(game)) error {
	file, err := dir.Open(filepath.Join(".enrichment", "gamelist.xml"))
	if err != nil {
		return err
	}
	defer file.Close()
	var list gameList
	if err := xml.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&list); err != nil {
		return fmt.Errorf("parse metadata preview: %w", err)
	}
	for _, g := range list.Games {
		rom, err := relativeScraperAsset(abs, scrapeRoot, g.Path)
		if err != nil {
			return err
		}
		info, err := romDir.Stat(rom)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("preview game is not a regular file: %s", rom)
		}
		publish(game{Path: "./" + filepath.ToSlash(rom), Name: g.Name, Other: textualMetadata(g.Other)})
	}
	return nil
}

// ES exporters may replace marquees with wheel logos. Pegasus preserves both types.
func mergeLogos(dir *os.Root, abs, scrapeRoot, dataAbs, scrapeData string) error {
	f, err := dir.Open(filepath.Join(".logos", "metadata.pegasus.txt"))
	if err != nil {
		return err
	}
	defer f.Close()
	resolve := func(value string) string {
		if scrapeRoot != abs {
			if path.IsAbs(value) {
				return value
			}
			return path.Join(scrapeData, ".logos", value)
		}
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(dataAbs, ".logos", filepath.FromSlash(value))
	}
	type logos struct{ wheel, marquee string }
	assets := map[string]*logos{}
	var current *logos
	scanner := bufio.NewScanner(io.LimitReader(f, 32<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "game:") {
			current = nil
		}
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "file":
			rom, err := relativeScraperAsset(abs, scrapeRoot, resolve(value))
			if err != nil {
				return err
			}
			current = &logos{}
			assets[rom] = current
		case "assets.wheel", "assets.marquee":
			if current == nil {
				return fmt.Errorf("logo asset without a game file in Pegasus export")
			}
			if key == "assets.wheel" {
				current.wheel = resolve(value)
			} else {
				current.marquee = resolve(value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	raw, err := dir.Open(filepath.Join(".enrichment", "gamelist.xml"))
	if err != nil {
		return err
	}
	var list gameList
	err = xml.NewDecoder(io.LimitReader(raw, 32<<20)).Decode(&list)
	raw.Close()
	if err != nil {
		return err
	}
	for i := range list.Games {
		g := &list.Games[i]
		rom, err := relativeScraperAsset(abs, scrapeRoot, g.Path)
		if err != nil {
			return err
		}
		logo, ok := assets[rom]
		if !ok {
			return fmt.Errorf("game missing from Pegasus export: %s", g.Path)
		}
		g.Wheel, g.Marquee = logo.wheel, logo.marquee
	}
	data, err := xml.Marshal(list)
	if err != nil {
		return err
	}
	return atomicBytes(dir, filepath.Join(".enrichment", "gamelist.xml"), data)
}

func buildProfiles(ctx context.Context, dir, romDir *os.Root, abs, scrapeRoot, dataAbs, scrapeData string, publish func(game)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := dir.Open(filepath.Join(".enrichment", "gamelist.xml"))
	if err != nil {
		return err
	}
	var original gameList
	err = xml.NewDecoder(io.LimitReader(f, 32<<20)).Decode(&original)
	f.Close()
	if err != nil {
		return fmt.Errorf("parse Skyscraper gamelist: %w", err)
	}
	composites := map[string]string{}
	compositeFile, err := dir.Open(filepath.Join(".composite", "gamelist.xml"))
	if err != nil {
		return err
	}
	var compositeList gameList
	err = xml.NewDecoder(io.LimitReader(compositeFile, 32<<20)).Decode(&compositeList)
	compositeFile.Close()
	if err != nil {
		return err
	}
	for _, g := range compositeList.Games {
		composites[g.Path] = g.Image
	}
	for _, profile := range []struct {
		name, manifest string
		width, height  int
	}{
		{"batocera", "gamelist.xml", 1280, 960},
		{"miyoo", "miyoogamelist.xml", 640, 480},
		{"tvos", "tvosgamelist.xml", 1920, 1080},
	} {
		if err := dir.MkdirAll(filepath.Join("media", profile.name), 0755); err != nil {
			return err
		}
		result := gameList{}
		for _, originalGame := range original.Games {
			if err := ctx.Err(); err != nil {
				return err
			}
			g := originalGame
			rom, err := relativeScraperAsset(abs, scrapeRoot, g.Path)
			if err != nil {
				return err
			}
			info, err := romDir.Stat(rom)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("game is not a regular file: %s", rom)
			}
			g.Path = "./" + filepath.ToSlash(rom)
			g.Image = composites[originalGame.Path]
			g.Screenshot = originalGame.Image
			g.Other = nil
			fields := []struct {
				field *string
				kind  string
			}{
				{&g.Image, "composite"}, {&g.Thumbnail, "boxart"}, {&g.Screenshot, "screenshot"},
				{&g.Wheel, "wheel"}, {&g.Marquee, "marquee"}, {&g.Fanart, "fanart"},
				{&g.Video, "video"}, {&g.Manual, "manual"},
			}
			for _, asset := range fields {
				if *asset.field == "" {
					continue
				}
				src, err := generatedAsset(abs, scrapeRoot, dataAbs, scrapeData, *asset.field)
				if err != nil {
					return err
				}
				extension := ".png"
				if asset.kind == "video" || asset.kind == "manual" {
					extension = strings.ToLower(filepath.Ext(src))
					if extension == "" {
						return fmt.Errorf("asset has no extension: %s", src)
					}
				}
				dest := filepath.Join("media", profile.name, asset.kind, digest(filepath.ToSlash(rom))+extension)
				if err := dir.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					return err
				}
				if asset.kind == "video" || asset.kind == "manual" {
					err = copyAsset(dir, src, dest)
				} else {
					err = resizePNG(dir, src, dest, profile.width, profile.height)
				}
				if err != nil {
					return fmt.Errorf("profile %s asset %s: %w", profile.name, src, err)
				}
				*asset.field = "./" + filepath.ToSlash(dest)
			}
			g.Other = textualMetadata(originalGame.Other)
			result.Games = append(result.Games, g)
			if profile.name == "batocera" && publish != nil {
				publish(g)
			}
		}
		data, err := xml.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicBytes(dir, profile.manifest, append([]byte(xml.Header), data...)); err != nil {
			return err
		}
	}
	return nil
}

func generatedAsset(abs, scrapeRoot, dataAbs, scrapeData, name string) (string, error) {
	if scrapeRoot != abs {
		full := name
		if !path.IsAbs(full) {
			full = path.Join(scrapeRoot, name)
		}
		clean, base := path.Clean(full), strings.TrimSuffix(path.Clean(scrapeData), "/")+"/"
		if !strings.HasPrefix(clean, base) {
			return "", fmt.Errorf("asset outside generated data: %s", name)
		}
		return filepath.FromSlash(strings.TrimPrefix(clean, base)), nil
	}
	full := filepath.FromSlash(name)
	if !filepath.IsAbs(full) {
		full = filepath.Join(abs, full)
	}
	return relativeAsset(dataAbs, full)
}

func copyAsset(root *os.Root, src, dest string) error {
	input, err := root.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dest), "."+id+".tmp")
	output, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, copyErr := io.CopyBuffer(output, input, make([]byte, ChunkSize))
	syncErr, closeErr := output.Sync(), output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(tmp, dest); err != nil {
		return err
	}
	return syncDirectory(root, filepath.Dir(dest))
}
func relativeScraperAsset(abs, scrapeRoot, name string) (string, error) {
	// Linux exports may contain absolute /mnt/... paths while Go runs on Windows.
	if scrapeRoot != abs && path.IsAbs(name) {
		clean, root := path.Clean(name), strings.TrimSuffix(path.Clean(scrapeRoot), "/")+"/"
		if !strings.HasPrefix(clean, root) {
			return "", fmt.Errorf("scraper asset escapes platform directory: %s", name)
		}
		name = strings.TrimPrefix(clean, root)
	}
	return relativeAsset(abs, name)
}

func relativeAsset(abs, name string) (string, error) {
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(abs, name)
		if err != nil {
			return "", err
		}
		name = rel
	}
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || filepath.IsAbs(name) {
		return "", fmt.Errorf("asset escapes platform directory: %s", name)
	}
	return name, nil
}

func resizePNG(root *os.Root, src, dest string, maxWidth, maxHeight int) error {
	f, err := root.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return fmt.Errorf("image exceeds 40 megapixel limit")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	width, height := cfg.Width, cfg.Height
	if width > maxWidth {
		height = max(1, height*maxWidth/width)
		width = maxWidth
	}
	if height > maxHeight {
		width = max(1, width*maxHeight/height)
		height = maxHeight
	}
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(resized, resized.Bounds(), img, img.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if err := png.Encode(&buf, resized); err != nil {
		return err
	}
	return atomicBytes(root, dest, buf.Bytes())
}

func atomicBytes(root *os.Root, name string, data []byte) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), "."+id+".tmp")
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return syncDirectory(root, filepath.Dir(name))
}
