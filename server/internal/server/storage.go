package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	pb "github.com/nlutsch/romsync/gen/romsync/v1"
)

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}

func readJSON(root *os.Root, name string, value any) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

func writeJSON(root *os.Root, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(name), "."+id+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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

func syncDirectory(root *os.Root, name string) error {
	// Windows does not support syncing directory handles through os.File.Sync.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type Device struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Profile   int32              `json:"profile"`
	TokenHash string             `json:"token_hash"`
	Approved  bool               `json:"approved"`
	Assets    *pb.AssetSelection `json:"assets,omitempty"`
}

func OpenState(path string) (*os.Root, error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"devices", "saves", "backups"} {
		if err := root.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			root.Close()
			return nil, err
		}
	}
	if err := syncDirectory(root, "."); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func Devices(root *os.Root) ([]Device, error) {
	dir, err := root.Open("devices")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	var result []Device
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		var d Device
		if err := readJSON(root, filepath.Join("devices", name), &d); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}

func Approve(root *os.Root, id string, approved bool) error {
	if !validDigest(id) {
		return fmt.Errorf("invalid device ID")
	}
	name := filepath.Join("devices", id+".json")
	var d Device
	if err := readJSON(root, name, &d); err != nil {
		return err
	}
	d.Approved = approved
	return writeJSON(root, name, d)
}
