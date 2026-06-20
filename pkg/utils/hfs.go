package utils

import (
	"embed"
	"io/fs"
	"os"
)

// HybridFS combines embed.FS and os.DirFS.
type HybridFS struct {
	embedFS, dir fs.FS
}

func NewHybridFS(embed embed.FS, subDir string, localDir string) (*HybridFS, error) {
	subFS, err := fs.Sub(embed, subDir)
	if err != nil {
		return nil, err
	}

	return &HybridFS{
		embedFS: subFS,
		dir:     os.DirFS(localDir),
	}, nil
}

func (hfs *HybridFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	// Ensure embed files are not replaced
	if file, err := hfs.embedFS.Open(name); err == nil {
		return file, nil
	}

	return hfs.dir.Open(name)
}
