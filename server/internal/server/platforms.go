package server

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

var platformAliases = map[string]string{
	"sfc": "snes", "super nintendo": "snes", "super famicom": "snes",
	"super nintendo entertainment system": "snes",
	"fc":                                  "nes", "famicom": "nes", "nintendo entertainment system": "nes",
	"game boy": "gb", "gameboy": "gb",
	"game boy color": "gbc", "gameboy color": "gbc",
	"game boy advance": "gba", "gameboy advance": "gba",
	"nintendo 64": "n64", "nintendo64": "n64",
	"ps": "psx", "ps1": "psx", "playstation": "psx",
	"md": "megadrive", "genesis": "megadrive", "mega drive": "megadrive",
	"sega genesis": "megadrive", "sega mega drive": "megadrive",
	"master system": "mastersystem", "sms": "mastersystem",
	"game gear": "gamegear", "gg": "gamegear",
	"pce": "pcengine", "pc engine": "pcengine", "turbografx16": "pcengine",
	"neo geo": "neogeo", "game cube": "gamecube",
}

var platformPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validPlatform(name string) bool { return platformPattern.MatchString(name) }

func canonicalPlatform(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if alias := platformAliases[name]; alias != "" {
		name = alias
	}
	if !validPlatform(name) || name == generatedDir {
		return "", fmt.Errorf("invalid system name %q: use a platform identifier or common system alias", name)
	}
	return name, nil
}

func platformFolders(root *os.Root) (map[string]string, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	folders := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.EqualFold(entry.Name(), generatedDir) {
			continue
		}
		platform, err := canonicalPlatform(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("ROM directory %q: %w", entry.Name(), err)
		}
		if previous, exists := folders[platform]; exists {
			return nil, fmt.Errorf("directories %q and %q both map to %q; keep one directory for this system", previous, entry.Name(), platform)
		}
		folders[platform] = entry.Name()
	}
	return folders, nil
}

func resolvePlatform(root *os.Root, name string) (string, string, error) {
	platform, err := canonicalPlatform(name)
	if err != nil {
		return "", "", err
	}
	folders, err := platformFolders(root)
	if err != nil {
		return "", "", err
	}
	folder, exists := folders[platform]
	if !exists {
		return platform, "", fmt.Errorf("system directory %q not found: %w", platform, fs.ErrNotExist)
	}
	return platform, folder, nil
}
