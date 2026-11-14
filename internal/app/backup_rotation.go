package app

import (
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Classify only archives with a readable Kafedra manifest. Unknown or damaged
// archives are never removed automatically on the assumption they are usable.
func backupCompleteness(path string) (complete, known bool) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return false, false
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name != "manifest.json" {
			continue
		}
		if f.UncompressedSize64 > 1<<20 {
			return false, false
		}
		r, err := f.Open()
		if err != nil {
			return false, false
		}
		defer r.Close()
		var m backupManifest
		if json.NewDecoder(io.LimitReader(r, (1<<20)+1)).Decode(&m) != nil || m.Product != "kafedra" {
			return false, false
		}
		return len(m.Damaged) == 0, true
	}
	return false, false
}

func (a *App) rotateBackups() {
	if a.Cfg.BackupKeep <= 0 {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(a.Cfg.DataPath(), "backup", "kafedra-*.zip"))
	var full, partial []string
	for _, path := range paths {
		complete, known := backupCompleteness(path)
		if !known {
			continue
		}
		if complete {
			full = append(full, path)
		} else {
			partial = append(partial, path)
		}
	}
	// Incomplete copies have their own retention quota and cannot displace full copies.
	for _, paths := range [][]string{full, partial} {
		sort.Strings(paths)
		for len(paths) > a.Cfg.BackupKeep {
			os.Remove(paths[0])
			paths = paths[1:]
		}
	}
}
