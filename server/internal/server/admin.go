package server

import (
	"context"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
)

//go:embed web/*
var adminAssets embed.FS

type AdminOptions struct {
	Control         *Control
	MaxROMBytes     int64
	Skyscraper      string
	WSLDistribution string
	Source          string
	Timeout         time.Duration
}

type Admin struct {
	server         *Server
	control        *Control
	maxROM         int64
	jobs           *jobQueue
	handler        http.Handler
	metadataMu     sync.Mutex
	scanRevisions  map[string]string
	attempts       map[string]metadataAttempt
	source         string
	previewMu      sync.RWMutex
	previews       map[string]game
	previewAssets  map[string]bool
	previewVersion uint64
}

type metadataAttempt struct {
	revision string
	job      MetadataJob
}

func NewAdmin(ctx context.Context, s *Server, options AdminOptions) (*Admin, error) {
	if options.Control == nil {
		return nil, fmt.Errorf("dashboard control is required")
	}
	if options.MaxROMBytes <= 0 || options.MaxROMBytes > 1<<40 {
		return nil, fmt.Errorf("max ROM bytes must be between 1 and 1 TiB")
	}
	if options.Timeout <= 0 || options.Skyscraper == "" || options.Source == "" {
		return nil, fmt.Errorf("metadata timeout, executable and source are required")
	}
	a := &Admin{server: s, control: options.Control, maxROM: options.MaxROMBytes,
		scanRevisions: make(map[string]string), attempts: make(map[string]metadataAttempt), source: options.Source,
		previews: make(map[string]game), previewAssets: make(map[string]bool)}
	a.jobs = newJobQueue(ctx, func(ctx context.Context, platform string, out io.Writer) error {
		ctx, cancel := context.WithTimeout(ctx, options.Timeout)
		defer cancel()
		a.metadataMu.Lock()
		revision := a.scanRevisions[platform]
		a.metadataMu.Unlock()
		a.clearPreviews(platform)
		s.log.Info("metadata job started", "platform", platform)
		err := Enrich(ctx, s.roms.Name(), platform, ScraperOptions{Binary: options.Skyscraper, Source: options.Source, WSLDistribution: options.WSLDistribution,
			progress: func(g game) { a.publishPreview(platform, g) }}, out)
		if ctx.Err() != nil {
			s.log.Error("metadata job cancelled", "platform", platform, "error", ctx.Err())
			return errors.Join(err, ctx.Err())
		}
		scanErr := s.scan(ctx)
		err = errors.Join(err, scanErr)
		if err == nil && revision != "" {
			err = s.markMetadata(platform, digest(revision+"\x00"+options.Source))
		}
		if err != nil {
			s.log.Error("metadata job failed", "platform", platform, "error", err)
		} else {
			a.clearPreviews(platform)
			s.log.Info("metadata job completed", "platform", platform)
		}
		return err
	})
	s.scanMu.Lock()
	s.onPlatformScanned = a.platformScanned
	s.scanMu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/library", a.library)
	mux.HandleFunc("GET /api/devices", a.devices)
	mux.HandleFunc("POST /api/devices/{id}", a.approval)
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) { a.json(w, http.StatusOK, a.jobs.snapshot()) })
	mux.HandleFunc("POST /api/jobs", a.startJob)
	mux.HandleFunc("PUT /api/roms", a.upload)
	mux.HandleFunc("GET /api/media", a.media)
	a.handler = mux
	return a, nil
}

func previewAssetIDs(platform string, g game) map[string]string {
	assets := make(map[string]string)
	for _, asset := range []struct{ kind, value string }{
		{"composite", g.Image}, {"boxart", g.Thumbnail}, {"screenshot", g.Screenshot},
		{"wheel", g.Wheel}, {"marquee", g.Marquee}, {"fanart", g.Fanart},
		{"video", g.Video}, {"manual", g.Manual},
	} {
		if asset.value != "" {
			assets[asset.kind] = path.Join(platform, asset.value)
		}
	}
	return assets
}

func (a *Admin) publishPreview(platform string, g game) {
	a.previewMu.Lock()
	defer a.previewMu.Unlock()
	a.previews[path.Join(platform, g.Path)] = g
	for _, id := range previewAssetIDs(platform, g) {
		a.previewAssets[id] = true
	}
	a.previewVersion++
}

func (a *Admin) clearPreviews(platform string) {
	a.previewMu.Lock()
	defer a.previewMu.Unlock()
	for id := range a.previews {
		if strings.HasPrefix(id, platform+"/") {
			delete(a.previews, id)
		}
	}
	for id := range a.previewAssets {
		if strings.HasPrefix(id, platform+"/") {
			delete(a.previewAssets, id)
		}
	}
	a.previewVersion++
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.control.ServeHTTP(w, r) }
func (a *Admin) Close() {
	a.jobs.close()
	a.server.scanMu.Lock()
	a.server.onPlatformScanned = nil
	a.server.scanMu.Unlock()
}

func (a *Admin) platformScanned(platform, revision, completed string) {
	a.metadataMu.Lock()
	defer a.metadataMu.Unlock()
	a.scanRevisions[platform] = revision
	target := digest(revision + "\x00" + a.source)
	if completed == target || a.attempts[platform].revision == target {
		return
	}
	job, err := a.jobs.enqueue(platform)
	if err != nil {
		a.server.log.Error("automatic metadata job not queued", "platform", platform, "error", err)
		return
	}
	a.attempts[platform] = metadataAttempt{revision: target, job: job}
	a.server.log.Info("metadata queued after system scan", "platform", platform)
}

func (a *Admin) json(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		a.server.log.Error("admin response failed", "error", err)
	}
}
func (a *Admin) fail(w http.ResponseWriter, code int, message string) {
	a.json(w, code, map[string]string{"error": message})
}
func (a *Admin) storageError(w http.ResponseWriter, err error) {
	a.server.log.Error("admin storage operation failed", "error", err)
	a.fail(w, http.StatusInternalServerError, "server storage operation failed; see server logs")
}
func (a *Admin) decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		a.fail(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		a.fail(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func (a *Admin) devices(w http.ResponseWriter, r *http.Request) {
	devices, err := Devices(a.server.state)
	if err != nil {
		a.storageError(w, err)
		return
	}
	type deviceView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Profile  string `json:"profile"`
		Approved bool   `json:"approved"`
	}
	result := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		result = append(result, deviceView{d.ID, d.Name, pb.Profile(d.Profile).String(), d.Approved})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	a.json(w, http.StatusOK, result)
}

func (a *Admin) approval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approved *bool `json:"approved"`
	}
	if !a.decode(w, r, &req) {
		return
	}
	if !validDigest(r.PathValue("id")) || req.Approved == nil {
		a.fail(w, http.StatusBadRequest, "valid device ID and approved boolean required")
		return
	}
	a.server.mu.Lock()
	err := Approve(a.server.state, r.PathValue("id"), *req.Approved)
	a.server.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		a.fail(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		a.storageError(w, err)
		return
	}
	a.server.log.Info("device approval updated", "id", r.PathValue("id"), "approved", *req.Approved)
	a.json(w, http.StatusOK, map[string]bool{"approved": *req.Approved})
}

func (a *Admin) startJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
	}
	if !a.decode(w, r, &req) {
		return
	}
	if _, err := canonicalPlatform(req.Platform); err != nil {
		a.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	platform, _, err := resolvePlatform(a.server.roms, req.Platform)
	if errors.Is(err, os.ErrNotExist) {
		a.fail(w, http.StatusNotFound, "platform folder not found")
		return
	}
	if err != nil {
		a.storageError(w, err)
		return
	}
	a.server.catalogMu.RLock()
	var entries []*pb.CatalogEntry
	for _, entry := range a.server.catalog.Entries {
		if entry.Platform == platform && entry.Kind == "rom" {
			entries = append(entries, entry)
		}
	}
	a.server.catalogMu.RUnlock()
	revision := romRevision(entries)
	a.metadataMu.Lock()
	a.scanRevisions[platform] = revision
	job, err := a.jobs.enqueue(platform)
	if err == nil {
		a.attempts[platform] = metadataAttempt{revision: digest(revision + "\x00" + a.source), job: job}
	}
	a.metadataMu.Unlock()
	if errors.Is(err, errQueueFull) {
		a.fail(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		a.storageError(w, err)
		return
	}
	a.json(w, http.StatusAccepted, job)
}

type adminGame struct {
	ID       string            `json:"id"`
	Platform string            `json:"platform"`
	Title    string            `json:"title"`
	Size     int64             `json:"size"`
	Checksum string            `json:"sha256"`
	Image    string            `json:"image,omitempty"`
	Metadata map[string]string `json:"metadata"`
	Assets   map[string]string `json:"assets"`
}

func (a *Admin) library(w http.ResponseWriter, r *http.Request) {
	a.server.catalogMu.RLock()
	catalog := a.server.catalog
	scanError := a.server.scanError
	a.server.catalogMu.RUnlock()
	platforms := map[string]bool{}
	mediaIDs := map[string]bool{}
	for _, entry := range catalog.Entries {
		platforms[entry.Platform] = true
		if entry.Kind == "media" && strings.HasSuffix(entry.Id, ".png") {
			mediaIDs[entry.Id] = true
		}
	}
	dirs, err := platformFolders(a.server.roms)
	if err != nil {
		a.storageError(w, err)
		return
	}
	for platform := range dirs {
		platforms[platform] = true
	}
	names := make([]string, 0, len(platforms))
	games := make([]adminGame, 0)
	scraped := map[string]game{}
	for platform := range platforms {
		names = append(names, platform)
		file, err := a.server.roms.Open(filepath.Join(generatedDir, platform, "gamelist.xml"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			a.storageError(w, err)
			return
		}
		var list gameList
		err = xml.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&list)
		file.Close()
		if err != nil {
			a.storageError(w, fmt.Errorf("read %s game list: %w", platform, err))
			return
		}
		for _, g := range list.Games {
			folder := dirs[platform]
			if folder == "" {
				folder = platform
			}
			rel, err := relativeAsset(filepath.Join(a.server.roms.Name(), folder), g.Path)
			if err != nil {
				a.storageError(w, err)
				return
			}
			scraped[path.Join(platform, filepath.ToSlash(rel))] = g
		}
	}
	sort.Strings(names)
	a.previewMu.RLock()
	previews := maps.Clone(a.previews)
	previewVersion := a.previewVersion
	a.previewMu.RUnlock()
	for id, preview := range previews {
		if len(previewAssetIDs(strings.SplitN(id, "/", 2)[0], preview)) != 0 {
			scraped[id] = preview
		} else {
			meta := scraped[id]
			if preview.Name != "" {
				meta.Name = preview.Name
			}
			meta.Path, meta.Other = preview.Path, preview.Other
			scraped[id] = meta
		}
	}
	for _, entry := range catalog.Entries {
		if entry.Kind != "rom" {
			continue
		}
		g := adminGame{ID: entry.Id, Platform: entry.Platform, Title: friendlyTitle(entry.Title), Size: entry.Size, Checksum: entry.Sha256, Metadata: map[string]string{}, Assets: map[string]string{}}
		if meta, ok := scraped[entry.Id]; ok {
			if name := strings.TrimSpace(meta.Name); name != "" {
				g.Title = friendlyTitle(name)
			}
			for _, e := range meta.Other {
				var value struct {
					Text string `xml:",chardata"`
				}
				if err := xml.Unmarshal([]byte("<value>"+e.Value+"</value>"), &value); err != nil {
					a.storageError(w, err)
					return
				}
				if e.XMLName.Local != "screenshot" {
					g.Metadata[e.XMLName.Local] = value.Text
				}
			}
			for _, asset := range catalog.Entries {
				if asset.GameId == entry.Id && asset.Kind == "media" && strings.Contains(asset.Id, "/media/batocera/") {
					g.Assets[assetNames[asset.AssetType]] = asset.Id
				}
			}
			if preview, exists := previews[entry.Id]; exists {
				for name, id := range previewAssetIDs(entry.Platform, preview) {
					g.Assets[name] = id
					if name == "composite" {
						g.Image = id
					}
				}
			}
			if meta.Image != "" {
				rel, err := relativeAsset(filepath.Join(a.server.roms.Name(), dirs[entry.Platform]), meta.Image)
				if err != nil {
					a.storageError(w, err)
					return
				}
				imageID := path.Join(entry.Platform, filepath.ToSlash(rel))
				if mediaIDs[imageID] {
					g.Image = imageID
				}
			}
		}
		games = append(games, g)
	}
	data, err := json.Marshal(games)
	if err != nil {
		a.storageError(w, err)
		return
	}
	revision := digest(fmt.Sprintf("%s\x00%d\x00%s", catalog.Revision, previewVersion, data))
	a.json(w, http.StatusOK, struct {
		Revision  string      `json:"revision"`
		Platforms []string    `json:"platforms"`
		Games     []adminGame `json:"games"`
		MaxROM    int64       `json:"max_rom_bytes"`
		Indexing  bool        `json:"indexing"`
		ScanError string      `json:"scan_error,omitempty"`
	}{revision, names, games, a.maxROM, catalog.Revision == "", scanError})
}

func (a *Admin) media(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a.server.catalogMu.RLock()
	found := false
	for _, entry := range a.server.catalog.Entries {
		if entry.Id == id && entry.Kind == "media" && strings.HasSuffix(id, ".png") {
			found = true
			break
		}
	}
	a.server.catalogMu.RUnlock()
	if !found && strings.HasSuffix(id, ".png") {
		a.previewMu.RLock()
		found = a.previewAssets[id]
		a.previewMu.RUnlock()
	}
	if !found {
		a.fail(w, http.StatusNotFound, "image not found")
		return
	}
	file, err := a.server.roms.Open(filepath.Join(generatedDir, filepath.FromSlash(id)))
	if err != nil {
		a.storageError(w, err)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		a.storageError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	http.ServeContent(w, r, path.Base(id), info.ModTime(), file)
}

var romExtensions = map[string]bool{
	".nes": true, ".sfc": true, ".smc": true, ".gb": true, ".gbc": true, ".gba": true,
	".n64": true, ".z64": true, ".v64": true, ".zip": true, ".7z": true, ".chd": true,
	".cue": true, ".bin": true, ".iso": true, ".rom": true, ".md": true, ".gen": true,
	".sms": true, ".gg": true, ".pce": true, ".cso": true, ".pbp": true, ".gdi": true,
}

func validROMFilename(name string) bool {
	return len(name) > 0 && len(name) <= 240 && utf8.ValidString(name) &&
		!strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".") &&
		strings.TrimSpace(name) == name && !strings.ContainsAny(name, "/\\:<>\"|?*") &&
		strings.IndexFunc(name, unicode.IsControl) < 0 && romExtensions[strings.ToLower(filepath.Ext(name))]
}

func (a *Admin) upload(w http.ResponseWriter, r *http.Request) {
	platform, name := r.URL.Query().Get("platform"), r.URL.Query().Get("name")
	platform, err := canonicalPlatform(platform)
	if err != nil || !validROMFilename(name) {
		a.fail(w, http.StatusBadRequest, "valid system name and a ROM filename without path separators required")
		return
	}
	if r.ContentLength > a.maxROM {
		a.fail(w, http.StatusRequestEntityTooLarge, "ROM exceeds upload limit")
		return
	}
	_, physical, err := resolvePlatform(a.server.roms, platform)
	if errors.Is(err, fs.ErrNotExist) {
		physical = platform
	} else if err != nil {
		a.storageError(w, err)
		return
	}
	if err := a.server.roms.Mkdir(physical, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		a.storageError(w, err)
		return
	}
	dir, err := a.server.roms.OpenRoot(physical)
	if err != nil {
		a.storageError(w, err)
		return
	}
	defer dir.Close()
	if _, err := dir.Lstat(name); err == nil {
		a.fail(w, http.StatusConflict, "filename already exists; existing ROM was not changed")
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		a.storageError(w, err)
		return
	}
	id, err := randomID()
	if err != nil {
		a.storageError(w, err)
		return
	}
	tmp := "." + id + ".upload"
	file, err := dir.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		a.storageError(w, err)
		return
	}
	defer dir.Remove(tmp)
	defer file.Close()
	r.Body = http.MaxBytesReader(w, r.Body, a.maxROM)
	size, err := io.CopyBuffer(file, r.Body, make([]byte, ChunkSize))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			a.fail(w, http.StatusRequestEntityTooLarge, "ROM exceeds upload limit")
		} else {
			a.server.log.Error("ROM upload interrupted", "error", err)
			a.fail(w, http.StatusBadRequest, "ROM upload interrupted")
		}
		return
	}
	if size == 0 {
		a.fail(w, http.StatusBadRequest, "empty ROM uploads are not allowed")
		return
	}
	if err := file.Sync(); err != nil {
		a.storageError(w, err)
		return
	}
	if err := file.Close(); err != nil {
		a.storageError(w, err)
		return
	}
	// Hard-link publication is atomic and never replaces an existing filename.
	if err := dir.Link(tmp, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			a.fail(w, http.StatusConflict, "filename already exists; existing ROM was not changed")
		} else {
			a.storageError(w, err)
		}
		return
	}
	var warnings []string
	if err := syncDirectory(dir, "."); err != nil {
		a.server.log.Error("uploaded ROM directory sync failed", "error", err)
		warnings = append(warnings, "ROM stored but directory sync failed; check server logs")
	}
	if err := a.server.Scan(); err != nil {
		a.server.log.Error("post-upload scan failed", "error", err)
		warnings = append(warnings, "ROM stored but catalog refresh failed; check server logs")
	}
	a.metadataMu.Lock()
	job := a.attempts[platform].job
	a.metadataMu.Unlock()
	if job.ID == "" {
		warnings = append(warnings, "ROM stored but metadata was not queued; check server logs")
	}
	a.server.log.Info("ROM uploaded", "platform", platform, "name", name, "size", size)
	a.json(w, http.StatusCreated, struct {
		ID       string      `json:"id"`
		Job      MetadataJob `json:"job"`
		Warnings []string    `json:"warnings"`
	}{path.Join(platform, name), job, warnings})
}
