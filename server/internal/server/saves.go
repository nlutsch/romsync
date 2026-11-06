package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type saveRecord struct {
	Revision string `json:"revision"`
	Checksum string `json:"sha256"`
	Size     int64  `json:"size"`
	File     string `json:"file"`
}

func (r saveRecord) proto() *pb.SaveRevision {
	return &pb.SaveRevision{Revision: r.Revision, Sha256: r.Checksum, Size: r.Size}
}

func (s *Server) gameAllowed(id string, profile pb.Profile) bool {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()
	for _, e := range s.catalog.Entries {
		if e.Id == id && e.Kind == "rom" && allowedPlatform(e.Platform, profile) {
			return true
		}
	}
	return false
}

func (s *Server) currentSave(id string) (saveRecord, error) {
	var r saveRecord
	err := readJSON(s.state, filepath.Join("saves", digest(id)+".json"), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	return r, err
}

func saveState(current saveRecord, base, checksum string) pb.SaveState {
	if checksum == current.Checksum {
		return pb.SaveState_UP_TO_DATE
	}
	if base == current.Revision {
		return pb.SaveState_CLIENT_IS_NEWER
	}
	if checksum == base || checksum == "" {
		return pb.SaveState_SERVER_IS_NEWER
	}
	return pb.SaveState_CONFLICT
}

func (s *Server) CheckSaveState(ctx context.Context, req *pb.CheckSaveStateRequest) (*pb.CheckSaveStateResponse, error) {
	d, err := s.authenticate(ctx)
	if err != nil {
		return nil, err
	}
	if !s.gameAllowed(req.GameId, pb.Profile(d.Profile)) {
		return nil, status.Error(codes.NotFound, "unknown game")
	}
	if (req.Sha256 != "" && !validDigest(req.Sha256)) || (req.BaseRevision != "" && !validDigest(req.BaseRevision)) {
		return nil, status.Error(codes.InvalidArgument, "invalid checksum or base revision")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.currentSave(req.GameId)
	if err != nil {
		return nil, s.internal(err)
	}
	return &pb.CheckSaveStateResponse{State: saveState(current, req.BaseRevision, req.Sha256), Server: current.proto()}, nil
}

func (s *Server) UploadSave(stream grpc.ClientStreamingServer[pb.FileChunk, pb.SaveRevision]) error {
	d, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "upload header required")
	}
	if err != nil {
		return err
	}
	if !s.gameAllowed(first.GameId, pb.Profile(d.Profile)) {
		return status.Error(codes.NotFound, "unknown game")
	}
	if !validDigest(first.Sha256) || (first.BaseRevision != "" && !validDigest(first.BaseRevision)) || len(first.Data) != 0 {
		return status.Error(codes.InvalidArgument, "first chunk must contain only game ID, checksum and optional base revision")
	}
	id, err := randomID()
	if err != nil {
		return s.internal(err)
	}
	tmp := filepath.Join("backups", "."+id+".tmp")
	f, err := s.state.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return s.internal(err)
	}
	defer s.state.Remove(tmp)
	defer f.Close()
	hash := sha256.New()
	var size int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(chunk.Data) == 0 || len(chunk.Data) > ChunkSize || chunk.GameId != "" || chunk.BaseRevision != "" || chunk.Sha256 != "" {
			return status.Error(codes.InvalidArgument, "data chunks must contain 1..65536 bytes and no header fields")
		}
		size += int64(len(chunk.Data))
		if size > s.maxSave {
			return status.Error(codes.ResourceExhausted, "save exceeds configured size limit")
		}
		if _, err := f.Write(chunk.Data); err != nil {
			return s.internal(err)
		}
		hash.Write(chunk.Data)
	}
	checksum := hex.EncodeToString(hash.Sum(nil))
	if checksum != first.Sha256 {
		return status.Error(codes.DataLoss, "save checksum mismatch")
	}
	if err := f.Sync(); err != nil {
		return s.internal(err)
	}
	if err := f.Close(); err != nil {
		return s.internal(err)
	}
	if _, err := s.authenticate(stream.Context()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.currentSave(first.GameId)
	if err != nil {
		return s.internal(err)
	}
	conflict := saveState(current, first.BaseRevision, checksum) == pb.SaveState_CONFLICT
	// Each upload is an immutable snapshot. The pointer is committed only after the snapshot is durable.
	file := filepath.Join("backups", digest(first.GameId)+"."+time.Now().UTC().Format("20060102T150405.000000000Z")+"."+id+".srm")
	if err := s.state.Rename(tmp, file); err != nil {
		return s.internal(err)
	}
	if err := syncDirectory(s.state, "backups"); err != nil {
		return s.internal(err)
	}
	record := saveRecord{Revision: checksum, Checksum: checksum, Size: size, File: file}
	if err := writeJSON(s.state, filepath.Join("saves", digest(first.GameId)+".json"), record); err != nil {
		return s.internal(err)
	}
	s.log.Info("save committed", "game", first.GameId, "device", d.ID, "conflict", conflict, "size", size)
	reply := record.proto()
	reply.ReplacedConflict = conflict
	return stream.SendAndClose(reply)
}

func (s *Server) DownloadSave(req *pb.DownloadSaveRequest, stream grpc.ServerStreamingServer[pb.FileChunk]) error {
	d, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	if !s.gameAllowed(req.GameId, pb.Profile(d.Profile)) {
		return status.Error(codes.NotFound, "unknown game")
	}
	s.mu.Lock()
	current, err := s.currentSave(req.GameId)
	s.mu.Unlock()
	if err != nil {
		return s.internal(err)
	}
	if current.Revision == "" {
		return status.Error(codes.NotFound, "no save exists")
	}
	if req.Revision != current.Revision {
		return status.Error(codes.FailedPrecondition, "check save state before downloading")
	}
	f, err := s.state.Open(current.File)
	if err != nil {
		return s.internal(err)
	}
	defer f.Close()
	return s.sendFile(f, current.Checksum, stream)
}
