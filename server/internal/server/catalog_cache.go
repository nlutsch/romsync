package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"strings"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/protobuf/proto"
)

type catalogFile struct {
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	SHA256  string `json:"sha256"`
}

type catalogCache struct {
	Version  int                    `json:"version"`
	ROMRoot  string                 `json:"rom_root"`
	Catalog  *pb.Catalog            `json:"catalog"`
	Files    map[string]catalogFile `json:"files"`
	Paths    map[string]string      `json:"paths"`
	Metadata map[string]string      `json:"metadata"`
}

func (s *Server) loadCatalog() error {
	file, err := s.state.Open("catalog.json")
	if err != nil {
		return err
	}
	defer file.Close()
	var cache catalogCache
	decoder := json.NewDecoder(io.LimitReader(file, 256<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cache); err != nil {
		return fmt.Errorf("read catalog cache: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("catalog cache must contain one JSON object")
	}
	if cache.Version != 1 || cache.Catalog == nil || cache.Files == nil || cache.Paths == nil || cache.Metadata == nil || !validDigest(cache.Catalog.Revision) {
		return fmt.Errorf("invalid catalog cache")
	}
	if cache.ROMRoot != s.roms.Name() {
		s.log.Info("catalog cache belongs to another ROM root; rebuilding")
		return fs.ErrNotExist
	}
	seen := make(map[string]bool)
	for _, entry := range cache.Catalog.Entries {
		if entry == nil || !fs.ValidPath(entry.Id) || !fs.ValidPath(cache.Paths[entry.Id]) || !validDigest(entry.Sha256) || entry.Size < 0 {
			return fmt.Errorf("invalid catalog cache entry")
		}
		platform, err := canonicalPlatform(entry.Platform)
		if err != nil || platform != entry.Platform || !fs.ValidPath(entry.TargetPath) || entry.Id != path.Join(platform, entry.TargetPath) || seen[entry.Id] {
			return fmt.Errorf("invalid or duplicate catalog identifier")
		}
		seen[entry.Id] = true
		physical := cache.Paths[entry.Id]
		if entry.Kind == "rom" {
			parts := strings.SplitN(physical, "/", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid cached ROM path")
			}
			system, err := canonicalPlatform(parts[0])
			if err != nil || system != platform || parts[1] != entry.TargetPath {
				return fmt.Errorf("invalid cached ROM path")
			}
		} else if (entry.Kind != "media" && entry.Kind != "metadata") || physical != path.Join(generatedDir, entry.Id) {
			return fmt.Errorf("invalid cached generated path")
		}
		file, exists := cache.Files[physical]
		if !exists || file.Size != entry.Size || file.SHA256 != entry.Sha256 {
			return fmt.Errorf("inconsistent catalog file cache")
		}
	}
	if len(cache.Files) != len(seen) || len(cache.Paths) != len(seen) {
		return fmt.Errorf("inconsistent catalog cache paths")
	}
	for name, file := range cache.Files {
		if !fs.ValidPath(name) || !validDigest(file.SHA256) || file.Size < 0 {
			return fmt.Errorf("invalid catalog file cache")
		}
	}
	s.cache, s.catalog, s.physicalPaths = cache, cache.Catalog, cache.Paths
	s.log.Info("catalog loaded", "entries", len(cache.Catalog.Entries))
	return nil
}

func (s *Server) persistCatalog(catalog *pb.Catalog, files map[string]catalogFile, paths map[string]string) error {
	next := s.cache
	next.Catalog, next.Files, next.Paths = catalog, files, paths
	if !proto.Equal(s.cache.Catalog, catalog) || !maps.Equal(s.cache.Files, files) || !maps.Equal(s.cache.Paths, paths) {
		if err := writeJSON(s.state, "catalog.json", next); err != nil {
			return fmt.Errorf("persist catalog: %w", err)
		}
	}
	s.cache = next
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	s.physicalPaths = paths
	changed := s.catalog.Revision != catalog.Revision
	s.catalog = catalog
	if changed {
		close(s.changed)
		s.changed = make(chan struct{})
		s.log.Info("catalog updated", "entries", len(catalog.Entries))
	}
	return nil
}

func (s *Server) markMetadata(platform, revision string) error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	previous := s.cache.Metadata[platform]
	s.cache.Metadata[platform] = revision
	if err := writeJSON(s.state, "catalog.json", s.cache); err != nil {
		s.cache.Metadata[platform] = previous
		return err
	}
	return nil
}

func (s *Server) physicalPath(entry *pb.CatalogEntry) string {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	if name := s.physicalPaths[entry.Id]; name != "" {
		return name
	}
	return catalogPhysicalPath(entry)
}

func (s *Server) initializeCache() {
	s.cache = catalogCache{Version: 1, ROMRoot: s.roms.Name(), Catalog: &pb.Catalog{},
		Files: make(map[string]catalogFile), Paths: make(map[string]string), Metadata: make(map[string]string)}
	s.catalog, s.physicalPaths = s.cache.Catalog, s.cache.Paths
	if err := s.loadCatalog(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("catalog cache could not be loaded; rebuilding in background", "error", err)
	}
}
