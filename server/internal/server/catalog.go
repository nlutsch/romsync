package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) Scan() error {
	return s.scan(context.Background())
}

type scanReader struct {
	ctx context.Context
	io.Reader
}

func (r scanReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (s *Server) scan(ctx context.Context) (result error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	defer func() {
		s.catalogMu.Lock()
		defer s.catalogMu.Unlock()
		s.scanError = ""
		if result != nil {
			s.scanError = result.Error()
		}
	}()
	previousFiles := s.cache.Files
	previousSystems := make(map[string]bool)
	for _, entry := range s.cache.Catalog.Entries {
		if entry.Kind == "rom" {
			previousSystems[entry.Platform] = true
		}
	}
	cold := s.cache.Catalog.Revision == ""
	catalog := &pb.Catalog{}
	hash := sha256.New()
	folders, err := platformFolders(s.roms)
	if err != nil {
		return fmt.Errorf("scan catalog: %w", err)
	}
	files := make(map[string]catalogFile)
	paths := make(map[string]string)
	scanDirectory := func(directory, canonical string) error {
		return fs.WalkDir(s.roms.FS(), directory, func(name string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if name != "." && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			parts := strings.Split(name, "/")
			if len(parts) < 2 {
				return nil
			}
			generated := parts[0] == generatedDir
			id, platform, kind := path.Join(canonical, strings.Join(parts[1:], "/")), canonical, "rom"
			assetType := pb.AssetType_ASSET_UNSPECIFIED
			target := strings.Join(parts[1:], "/")
			if generated {
				// Publish only generated manifests and profile assets, never raw exports/cache.
				if len(parts) < 3 {
					return nil
				}
				id, platform = path.Join(canonical, strings.Join(parts[2:], "/")), canonical
				switch {
				case len(parts) == 3 && (parts[2] == "gamelist.xml" || parts[2] == "miyoogamelist.xml" || parts[2] == "tvosgamelist.xml"):
					kind, target = "metadata", parts[2]
				case len(parts) == 6 && parts[2] == "media" && assetTypeFor(parts[4]) != pb.AssetType_ASSET_UNSPECIFIED:
					kind, target, assetType = "media", strings.Join(parts[2:], "/"), assetTypeFor(parts[4])
				default:
					return nil
				}
			} else {
				if strings.HasSuffix(strings.ToLower(name), ".xml") || (len(parts) > 2 && parts[1] == "media") {
					return nil
				}
			}
			cached, unchanged := previousFiles[name]
			unchanged = unchanged && cached.Size == info.Size() && cached.ModTime == info.ModTime().UnixNano()
			if !unchanged {
				f, err := s.roms.Open(filepath.FromSlash(name))
				if err != nil {
					return err
				}
				h := sha256.New()
				size, readErr := io.Copy(h, scanReader{ctx, f})
				after, statErr := f.Stat()
				closeErr := f.Close()
				if err := errors.Join(readErr, statErr, closeErr); err != nil {
					return err
				}
				if size != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
					return fmt.Errorf("file %q changed during hashing; retry scan", name)
				}
				cached = catalogFile{Size: size, ModTime: info.ModTime().UnixNano(), SHA256: hex.EncodeToString(h.Sum(nil))}
			}
			files[name] = cached
			paths[id] = name
			entry := &pb.CatalogEntry{Id: id, Platform: platform, Title: strings.TrimSuffix(path.Base(name), path.Ext(name)), Size: cached.Size, Sha256: cached.SHA256, Kind: kind, AssetType: assetType, TargetPath: target}
			catalog.Entries = append(catalog.Entries, entry)
			fmt.Fprintf(hash, "%s\x00%s\x00%d\x00", id, entry.Sha256, entry.Size)
			return nil
		})
	}
	platforms := make([]string, 0, len(folders))
	for platform := range folders {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		start := len(catalog.Entries)
		if err := scanDirectory(folders[platform], platform); err != nil {
			return fmt.Errorf("scan %s: %w", platform, err)
		}
		if cold && len(catalog.Entries) > start {
			partial := &pb.Catalog{Revision: hex.EncodeToString(hash.Sum(nil)), Entries: append([]*pb.CatalogEntry(nil), catalog.Entries...)}
			if err := s.persistCatalog(partial, maps.Clone(files), maps.Clone(paths)); err != nil {
				return err
			}
		}
		if s.onPlatformScanned != nil && (len(catalog.Entries) > start || previousSystems[platform] || s.cache.Metadata[platform] != "") {
			revision := romRevision(catalog.Entries[start:])
			s.onPlatformScanned(platform, revision, s.cache.Metadata[platform])
		}
	}
	generatedFolders, err := fs.ReadDir(s.roms.FS(), generatedDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("scan generated catalog: %w", err)
	}
	for _, folder := range generatedFolders {
		if !folder.IsDir() || strings.HasPrefix(folder.Name(), ".") {
			continue
		}
		platform, err := canonicalPlatform(folder.Name())
		if err != nil {
			return err
		}
		if platform != folder.Name() {
			s.log.Warn("ignoring legacy generated metadata directory; rerun metadata sync", "directory", folder.Name(), "platform", platform)
			continue
		}
		if err := scanDirectory(path.Join(generatedDir, platform), platform); err != nil {
			return fmt.Errorf("scan generated %s: %w", platform, err)
		}
	}
	romIDs := map[string]string{}
	for _, entry := range catalog.Entries {
		if entry.Kind == "rom" {
			romIDs[entry.Platform+"/"+digest(entry.TargetPath)] = entry.Id
		}
	}
	for _, entry := range catalog.Entries {
		if entry.Kind == "media" {
			entry.GameId = romIDs[entry.Platform+"/"+strings.TrimSuffix(path.Base(entry.Id), path.Ext(entry.Id))]
		}
	}
	catalog.Revision = hex.EncodeToString(hash.Sum(nil))
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.persistCatalog(catalog, files, paths)
}

func romRevision(entries []*pb.CatalogEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		if entry.Kind == "rom" {
			fmt.Fprintf(hash, "%s\x00%s\x00%d\x00", entry.Id, entry.Sha256, entry.Size)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func allowedPlatform(platform string, profile pb.Profile) bool {
	if profile != pb.Profile_TVOS {
		return true
	}
	switch platform {
	case "nes", "snes", "gb", "gbc", "gba", "megadrive", "genesis", "mastersystem", "gamegear", "pcengine", "psx", "n64", "dreamcast":
		return true
	}
	return false
}

func filterCatalog(c *pb.Catalog, profile pb.Profile) *pb.Catalog {
	return filterCatalogAssets(c, profile, &pb.AssetSelection{Types: []pb.AssetType{pb.AssetType_COMPOSITE}})
}

func filterCatalogAssets(c *pb.Catalog, profile pb.Profile, selection *pb.AssetSelection) *pb.Catalog {
	out := &pb.Catalog{Revision: c.Revision}
	for _, e := range c.Entries {
		if !allowedPlatform(e.Platform, profile) {
			continue
		}
		parts := strings.Split(e.Id, "/")
		if e.Kind == "media" && (len(parts) < 4 || parts[2] != strings.ToLower(profile.String())) {
			continue
		}
		if e.Kind == "media" && !wants(selection, e.AssetType) {
			// Legacy entries without a type remain composite-only.
			if e.AssetType != pb.AssetType_ASSET_UNSPECIFIED || !wants(selection, pb.AssetType_COMPOSITE) {
				continue
			}
		}
		if e.Kind == "metadata" {
			target := "gamelist.xml"
			if profile == pb.Profile_MIYOO {
				target = "miyoogamelist.xml"
			}
			if profile == pb.Profile_TVOS {
				target = "tvosgamelist.xml"
			}
			if path.Base(e.Id) != target {
				continue
			}
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

func (s *Server) DownloadFile(req *pb.DownloadFileRequest, stream grpc.ServerStreamingServer[pb.FileChunk]) error {
	if err := validateSelection(req.Assets); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	d, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	s.catalogMu.RLock()
	current := s.catalog
	s.catalogMu.RUnlock()
	c, manifests, err := s.deviceCatalog(current, pb.Profile(d.Profile), selectionFor(d, req.Assets))
	if err != nil {
		return s.internal(err)
	}
	var entry *pb.CatalogEntry
	for _, e := range c.Entries {
		if e.Id == req.Id {
			entry = e
			break
		}
	}
	if entry == nil {
		return status.Error(codes.NotFound, "file not in device catalog")
	}
	if req.Sha256 != entry.Sha256 {
		return status.Error(codes.FailedPrecondition, "catalog checksum required; refresh catalog")
	}
	if data, ok := manifests[entry.Id]; ok {
		for len(data) > 0 {
			if _, err := s.authenticate(stream.Context()); err != nil {
				return err
			}
			n := min(ChunkSize, len(data))
			if err := stream.Send(&pb.FileChunk{Data: data[:n]}); err != nil {
				return err
			}
			data = data[n:]
		}
		return nil
	}
	f, err := s.roms.Open(s.physicalPath(entry))
	if err != nil {
		return s.internal(err)
	}
	defer f.Close()
	return s.sendFile(f, entry.Sha256, stream)
}

func (s *Server) sendFile(f *os.File, expected string, stream grpc.ServerStreamingServer[pb.FileChunk]) error {
	buf := make([]byte, ChunkSize)
	hash := sha256.New()
	for {
		if _, err := s.authenticate(stream.Context()); err != nil {
			return err
		}
		n, err := f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			if sendErr := stream.Send(&pb.FileChunk{Data: buf[:n]}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return s.internal(err)
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return status.Error(codes.Aborted, "file changed during transfer; discard data and retry")
	}
	return nil
}

func (s *Server) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	addDirs := func() error {
		return fs.WalkDir(s.roms.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return w.Add(filepath.Join(s.roms.Name(), filepath.FromSlash(name)))
			}
			return nil
		})
	}
	if err := addDirs(); err != nil {
		return err
	}
	s.log.Info("reconciling ROM library in background")
	if err := s.scan(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.log.Error("initial catalog reconciliation failed; will retry", "error", err)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-w.Errors:
			if !ok {
				return fmt.Errorf("filesystem watcher closed")
			}
			s.log.Error("filesystem watch error; periodic scan will reconcile", "error", err)
		case _, ok := <-w.Events:
			if !ok {
				return fmt.Errorf("filesystem events closed")
			}
			debounce.Reset(300 * time.Millisecond)
		case <-debounce.C:
			if err := addDirs(); err != nil {
				s.log.Error("watch directories", "error", err)
			}
			if err := s.scan(ctx); err != nil {
				s.log.Error("catalog scan failed", "error", err)
			}
		case <-ticker.C:
			if err := addDirs(); err != nil {
				s.log.Error("watch directories", "error", err)
			}
			if err := s.scan(ctx); err != nil {
				s.log.Error("catalog reconciliation failed", "error", err)
			}
		}
	}
}
