package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const ChunkSize = 64 * 1024

type Server struct {
	pb.UnimplementedRomSyncServer
	state, roms *os.Root
	log         *slog.Logger
	maxSave     int64
	mu          sync.Mutex
	scanMu      sync.Mutex
	catalogMu   sync.RWMutex
	catalog     *pb.Catalog
	changed     chan struct{}
}

func New(romPath, statePath string, maxSave int64, log *slog.Logger) (*Server, error) {
	if maxSave <= 0 {
		return nil, fmt.Errorf("max save bytes must be positive")
	}
	romAbs, err := filepath.Abs(romPath)
	if err != nil {
		return nil, err
	}
	romAbs, err = filepath.EvalSymlinks(romAbs)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(statePath, 0700); err != nil {
		return nil, err
	}
	stateAbs, err := filepath.Abs(statePath)
	if err != nil {
		return nil, err
	}
	stateAbs, err = filepath.EvalSymlinks(stateAbs)
	if err != nil {
		return nil, err
	}
	// State must never be published as part of the ROM tree.
	rel, err := filepath.Rel(romAbs, stateAbs)
	if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))) {
		return nil, fmt.Errorf("state directory must be outside ROM directory")
	}
	roms, err := os.OpenRoot(romAbs)
	if err != nil {
		return nil, err
	}
	state, err := OpenState(statePath)
	if err != nil {
		roms.Close()
		return nil, err
	}
	s := &Server{state: state, roms: roms, log: log, maxSave: maxSave, changed: make(chan struct{})}
	if err := s.Scan(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() error { return errors.Join(s.state.Close(), s.roms.Close()) }

func (s *Server) internal(err error) error {
	s.log.Error("request failed", "error", err)
	return status.Error(codes.Internal, "server storage operation failed")
}

func (s *Server) RegisterDevice(ctx context.Context, req *pb.RegisterDeviceRequest) (*pb.RegisterDeviceResponse, error) {
	if err := validateSelection(req.Assets); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(strings.TrimSpace(req.Name)) == 0 || len(req.Name) > 128 || req.Profile < pb.Profile_BATOCERA || req.Profile > pb.Profile_TVOS {
		return nil, status.Error(codes.InvalidArgument, "name and valid profile required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, err := Devices(s.state)
	if err != nil {
		return nil, s.internal(err)
	}
	if len(devices) >= 1024 {
		return nil, status.Error(codes.ResourceExhausted, "device registry full; administrator must remove unused registrations")
	}
	id, err := randomID()
	if err != nil {
		return nil, s.internal(err)
	}
	secret, err := randomID()
	if err != nil {
		return nil, s.internal(err)
	}
	token := id + "." + secret
	d := Device{ID: id, Name: req.Name, Profile: int32(req.Profile), TokenHash: digest(token), Assets: req.Assets}
	if err := writeJSON(s.state, filepath.Join("devices", id+".json"), d); err != nil {
		return nil, s.internal(err)
	}
	s.log.Info("device awaiting approval", "id", id, "name", req.Name)
	return &pb.RegisterDeviceResponse{DeviceId: id, Token: token}, nil
}

func (s *Server) authenticate(ctx context.Context) (*Device, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return nil, status.Error(codes.Unauthenticated, "Bearer token required")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	id, _, ok := strings.Cut(token, ".")
	if !ok || !validDigest(id) || len(token) != 129 {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	var d Device
	if err := readJSON(s.state, filepath.Join("devices", id+".json"), &d); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, s.internal(err)
		}
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if subtle.ConstantTimeCompare([]byte(digest(token)), []byte(d.TokenHash)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	if d.ID != id || d.Profile < int32(pb.Profile_BATOCERA) || d.Profile > int32(pb.Profile_TVOS) {
		return nil, s.internal(fmt.Errorf("invalid stored device record %s", id))
	}
	if !d.Approved {
		return nil, status.Error(codes.PermissionDenied, "device approval required")
	}
	return &d, nil
}

func (s *Server) WatchCatalog(req *pb.WatchCatalogRequest, stream grpc.ServerStreamingServer[pb.Catalog]) error {
	if err := validateSelection(req.Assets); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	for {
		d, err := s.authenticate(stream.Context())
		if err != nil {
			return err
		}
		s.catalogMu.RLock()
		catalog, changed := s.catalog, s.changed
		s.catalogMu.RUnlock()
		filtered, _, err := s.deviceCatalog(catalog, pb.Profile(d.Profile), selectionFor(d, req.Assets))
		if err != nil {
			return s.internal(err)
		}
		if err := stream.Send(filtered); err != nil {
			return err
		}
		// Recheck approval even when the catalog is unchanged.
		ticker := time.NewTicker(5 * time.Second)
	wait:
		for {
			select {
			case <-stream.Context().Done():
				ticker.Stop()
				return stream.Context().Err()
			case <-changed:
				ticker.Stop()
				break wait
			case <-ticker.C:
				if _, err := s.authenticate(stream.Context()); err != nil {
					ticker.Stop()
					return err
				}
			}
		}
	}
}
