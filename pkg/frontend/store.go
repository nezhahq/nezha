package frontend

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	maxArchiveSize   int64 = 64 << 20
	maxExtractedSize int64 = 256 << 20
	maxArchiveFiles        = 10000
)

type Template struct {
	Path       string `json:"path,omitempty"`
	Name       string `json:"name,omitempty"`
	Repository string `json:"repository,omitempty"`
	Author     string `json:"author,omitempty"`
	Version    string `json:"version,omitempty"`
	IsAdmin    bool   `json:"is_admin,omitempty"`
	IsOfficial bool   `json:"is_official,omitempty"`
	Custom     bool   `json:"custom,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type Store struct {
	root      string
	client    *Client
	mu        sync.RWMutex
	installMu sync.Mutex
	templates []Template
}

func NewStore(root string, builtins []Template) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	store := &Store{root: absolute, client: NewClient(), templates: append([]Template(nil), builtins...)}
	directory, err := os.OpenRoot(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := fs.ReadDir(directory.FS(), ".")
	if err != nil {
		return nil, err
	}
	// A broken optional installation must not prevent the dashboard from starting.
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := directory.ReadFile(filepath.Join(entry.Name(), "metadata.json"))
		if err != nil {
			log.Printf("NEZHA>> quarantining frontend %q: %v", entry.Name(), err)
			continue
		}
		var template Template
		if err := json.Unmarshal(data, &template); err != nil {
			log.Printf("NEZHA>> quarantining frontend %q: %v", entry.Name(), err)
			continue
		}
		if !template.Custom || template.Path != entry.Name() || template.IsOfficial {
			log.Printf("NEZHA>> quarantining frontend %q: invalid metadata", entry.Name())
			continue
		}
		if _, err := directory.Stat(filepath.Join(template.Path, "dist", "index.html")); err != nil {
			continue
		}
		store.templates = append(store.templates, template)
	}
	return store, nil
}

func (s *Store) Templates() []Template {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Template(nil), s.templates...)
}

func (s *Store) Directory(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, template := range s.templates {
		if template.Path == id && template.Custom {
			return filepath.Join(s.root, id, "dist")
		}
	}
	return id
}

func (s *Store) Install(ctx context.Context, repository, tag string, admin bool) (*Template, error) {
	api, err := RepositoryAPI(repository)
	if err != nil {
		return nil, err
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%t", api, tag, admin)))
	role := "user"
	if admin {
		role = "admin"
	}
	id := fmt.Sprintf("frontend-%s-%x", role, identity[:12])
	s.installMu.Lock()
	defer s.installMu.Unlock()
	for _, installed := range s.Templates() {
		if installed.Path == id {
			return &installed, nil
		}
	}
	release, err := s.client.release(ctx, repository, tag)
	if err != nil {
		return nil, err
	}
	asset, ok := distributionAsset(release)
	if !ok {
		return nil, errors.New("release must contain a dist.zip frontend asset")
	}
	if err := os.MkdirAll(s.root, 0750); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(s.root, ".frontend-install-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	stagingRoot, err := os.OpenRoot(staging)
	if err != nil {
		return nil, err
	}
	defer stagingRoot.Close()
	archivePath := filepath.Join(staging, "dist.zip")
	response, err := s.client.open(ctx, asset.URL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	archive, err := stagingRoot.OpenFile("dist.zip", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, digest), io.LimitReader(response.Body, maxArchiveSize+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if written > maxArchiveSize {
		return nil, errors.New("frontend archive exceeds 64 MiB")
	}
	if asset.Size > 0 && written != asset.Size {
		return nil, errors.New("frontend archive size does not match release metadata")
	}
	checksum := hex.EncodeToString(digest.Sum(nil))
	if asset.Digest != "" && asset.Digest != "sha256:"+checksum {
		return nil, errors.New("frontend archive checksum does not match release metadata")
	}
	if err := extractDistribution(ctx, archivePath, staging); err != nil {
		return nil, err
	}
	if err := stagingRoot.Remove("dist.zip"); err != nil {
		return nil, err
	}
	name := release.Name
	if name == "" {
		name = tag
	}
	template := Template{Path: id, Name: name, Repository: repository, Version: tag, IsAdmin: admin, Custom: true, SHA256: checksum}
	metadata, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := stagingRoot.WriteFile("metadata.json", metadata, 0600); err != nil {
		return nil, err
	}
	if err := stagingRoot.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Publish only a completely downloaded and validated directory. A failed install
	// must never replace files currently being served or leave a selectable template.
	if err := os.Rename(staging, filepath.Join(s.root, id)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.templates = append(s.templates, template)
	sort.SliceStable(s.templates, func(i, j int) bool { return s.templates[i].Path < s.templates[j].Path })
	s.mu.Unlock()
	return &template, nil
}

// Remove deletes an inactive custom installation; callers must serialize selection
// changes with this operation and reject currently selected templates.
func (s *Store) Remove(id string, admin bool) error {
	s.installMu.Lock()
	defer s.installMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never interpret caller-controlled paths as filesystem paths before matching
	// the catalog and validating a single directory component.
	if id == "." || id == ".." || strings.ContainsAny(id, "/\\") || id == "" {
		return errors.New("invalid frontend path")
	}
	for i, template := range s.templates {
		if template.Path != id || template.IsAdmin != admin {
			continue
		}
		if !template.Custom || template.IsOfficial {
			return errors.New("built-in frontends cannot be removed")
		}
		root, err := os.OpenRoot(s.root)
		if err != nil {
			return err
		}
		defer root.Close()
		if err := root.RemoveAll(id); err != nil {
			return err
		}
		s.templates = append(s.templates[:i], s.templates[i+1:]...)
		return nil
	}
	return errors.New("frontend is not installed for the selected role")
}

func extractDistribution(ctx context.Context, archivePath, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("invalid frontend ZIP: %w", err)
	}
	defer archive.Close()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer root.Close()
	if len(archive.File) > maxArchiveFiles {
		return errors.New("frontend archive contains too many files")
	}
	seen := make(map[string]bool)
	var extracted int64
	for _, entry := range archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") || (name != "dist" && !strings.HasPrefix(name, "dist/")) || path.Clean(name) != name || seen[name] {
			return errors.New("frontend archive must contain only safe paths beneath dist/")
		}
		seen[name] = true
		if entry.Mode()&os.ModeSymlink != 0 || (!entry.FileInfo().IsDir() && !entry.Mode().IsRegular()) {
			return errors.New("frontend archive cannot contain links or special files")
		}
		// Lexical ZIP validation alone cannot stop a pre-existing filesystem
		// symlink from escaping the destination. Keep every write inside os.Root.
		target := filepath.FromSlash(name)
		if entry.FileInfo().IsDir() {
			if err := root.MkdirAll(target, 0750); err != nil {
				return err
			}
			continue
		}
		if entry.UncompressedSize64 > uint64(maxExtractedSize-extracted) {
			return errors.New("extracted frontend exceeds 256 MiB")
		}
		if err := root.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output, err := root.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			input.Close()
			return err
		}
		count, copyErr := io.Copy(output, io.LimitReader(input, maxExtractedSize-extracted+1))
		input.Close()
		closeErr := output.Close()
		extracted += count
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if extracted > maxExtractedSize {
			return errors.New("extracted frontend exceeds 256 MiB")
		}
	}
	index, err := root.Stat(filepath.Join("dist", "index.html"))
	if err != nil || !index.Mode().IsRegular() || index.Size() == 0 {
		return errors.New("frontend archive must contain a nonempty dist/index.html")
	}
	return nil
}
