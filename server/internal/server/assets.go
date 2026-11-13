package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"path/filepath"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const generatedDir = "romsync-data"

var assetNames = map[pb.AssetType]string{
	pb.AssetType_COMPOSITE: "composite", pb.AssetType_BOXART: "boxart",
	pb.AssetType_SCREENSHOT: "screenshot", pb.AssetType_WHEEL: "wheel",
	pb.AssetType_MARQUEE: "marquee", pb.AssetType_FANART: "fanart",
	pb.AssetType_VIDEO: "video", pb.AssetType_MANUAL: "manual",
}

func validateSelection(selection *pb.AssetSelection) error {
	if selection == nil {
		return nil
	}
	if len(selection.Types) > len(assetNames) {
		return fmt.Errorf("too many asset types")
	}
	seen := map[pb.AssetType]bool{}
	for _, kind := range selection.Types {
		if assetNames[kind] == "" || seen[kind] {
			return fmt.Errorf("asset types must be valid and unique")
		}
		seen[kind] = true
	}
	return nil
}

func selectionFor(d *Device, override *pb.AssetSelection) *pb.AssetSelection {
	if override != nil {
		return override
	}
	if d.Assets != nil {
		return d.Assets
	}
	return &pb.AssetSelection{Types: []pb.AssetType{pb.AssetType_COMPOSITE}}
}

func wants(selection *pb.AssetSelection, kind pb.AssetType) bool {
	for _, value := range selection.Types {
		if value == kind {
			return true
		}
	}
	return false
}

func catalogPhysicalPath(entry *pb.CatalogEntry) string {
	if entry.Kind == "rom" {
		return filepath.FromSlash(entry.Id)
	}
	return filepath.Join(generatedDir, filepath.FromSlash(entry.Id))
}

func (s *Server) SetAssetPreferences(ctx context.Context, req *pb.SetAssetPreferencesRequest) (*pb.AssetSelection, error) {
	d, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if req.Assets == nil {
		return nil, status.Error(codes.InvalidArgument, "assets selection is required; empty types disables media")
	}
	if err := validateSelection(req.Assets); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reload approval state so a concurrent administrator update cannot be overwritten.
	if err := readJSON(s.state, filepath.Join("devices", d.ID+".json"), d); err != nil {
		return nil, s.internal(err)
	}
	if !d.Approved {
		return nil, status.Error(codes.PermissionDenied, "device approval required")
	}
	d.Assets = req.Assets
	if err := writeJSON(s.state, filepath.Join("devices", d.ID+".json"), d); err != nil {
		return nil, s.internal(err)
	}
	s.catalogMu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.catalogMu.Unlock()
	return req.Assets, nil
}

func assetTypeFor(name string) pb.AssetType {
	for kind, value := range assetNames {
		if value == name {
			return kind
		}
	}
	return pb.AssetType_ASSET_UNSPECIFIED
}

func assetTarget(profile, kind, filename string) string {
	return path.Join("media", profile, kind, filename)
}

func (s *Server) deviceCatalog(c *pb.Catalog, profile pb.Profile, selection *pb.AssetSelection) (*pb.Catalog, map[string][]byte, error) {
	filtered := filterCatalogAssets(c, profile, selection)
	out := proto.Clone(filtered).(*pb.Catalog)
	manifests := map[string][]byte{}
	media := map[string]*pb.CatalogEntry{}
	for _, entry := range out.Entries {
		if entry.Kind == "media" {
			media[entry.Id] = entry
		}
	}
	for _, entry := range out.Entries {
		if entry.Kind != "metadata" {
			continue
		}
		f, err := s.roms.Open(s.physicalPath(entry))
		if err != nil {
			return nil, nil, err
		}
		var list gameList
		err = xml.NewDecoder(io.LimitReader(f, 32<<20)).Decode(&list)
		f.Close()
		if err != nil {
			return nil, nil, err
		}
		for i := range list.Games {
			g := &list.Games[i]
			for _, field := range []struct {
				value *string
				kind  pb.AssetType
			}{
				{&g.Image, pb.AssetType_COMPOSITE}, {&g.Thumbnail, pb.AssetType_BOXART}, {&g.Screenshot, pb.AssetType_SCREENSHOT},
				{&g.Wheel, pb.AssetType_WHEEL}, {&g.Marquee, pb.AssetType_MARQUEE},
				{&g.Fanart, pb.AssetType_FANART}, {&g.Video, pb.AssetType_VIDEO}, {&g.Manual, pb.AssetType_MANUAL},
			} {
				asset := media[path.Join(entry.Platform, *field.value)]
				if asset == nil || asset.AssetType != field.kind || asset.GameId != path.Join(entry.Platform, g.Path) {
					*field.value = ""
				}
			}
			if g.Image == "" {
				g.Image, g.Screenshot = g.Screenshot, ""
			}
		}
		data, err := xml.Marshal(list)
		if err != nil {
			return nil, nil, err
		}
		h := sha256.Sum256(data)
		entry.Sha256, entry.Size = hex.EncodeToString(h[:]), int64(len(data))
		manifests[entry.Id] = data
	}
	data, err := proto.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	out.Revision = digest(string(data))
	return out, manifests, nil
}
