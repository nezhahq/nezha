package frontend

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func distributionZIP(t *testing.T, names ...string) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, "<html>installed frontend</html>"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestRepositoryAPI(t *testing.T) {
	for input, want := range map[string]string{
		"https://github.com/owner/site.git":                      "https://api.github.com/repos/owner/site",
		"https://gitee.com/owner/site/releases":                  "https://gitee.com/api/v5/repos/owner/site",
		"https://atomgit.com/owner/site":                         "https://api.atomgit.com/api/v5/repos/owner/site",
		"https://forge.example/api/v1/repos/owner/site/releases": "https://forge.example/api/v1/repos/owner/site",
		"https://forge.example/api/v3/repos/owner/site":          "https://forge.example/api/v3/repos/owner/site",
	} {
		got, err := RepositoryAPI(input)
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"http://github.com/owner/site", "https://token@github.com/owner/site", "https://github.com/owner/site?token=secret", "https://github.com/owner", "https://forge.example/owner/site", "https://github.com/../site", "https://github.com/owner/%2e%2e"} {
		if _, err := RepositoryAPI(input); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestReleaseDownloadsRejectPrivateAndUnencryptedURLs(t *testing.T) {
	for _, address := range []string{"https://127.0.0.1/archive.zip", "https://169.254.169.254/archive.zip", "http://github.com/archive.zip", "https://token@github.com/archive.zip"} {
		if response, err := openReleaseURL(context.Background(), address); err == nil {
			response.Body.Close()
			t.Errorf("accepted prohibited destination %s", address)
		}
	}
}

func TestRepositoryAPIRequiresOwnerAndRepository(t *testing.T) {
	for _, address := range []string{"https://forge.example/api/v1/repos//site", "https://forge.example/repos/owner//releases"} {
		if _, err := RepositoryAPI(address); err == nil {
			t.Errorf("accepted incomplete repository: %s", address)
		}
	}
}

func TestReleaseListFiltersDistributionAndPreservesPagination(t *testing.T) {
	releases := make([]Release, 20)
	releases[0] = Release{Tag: "v1.0.0", Assets: []Asset{{Name: "dist.zip", URL: "https://cdn.example/dist.zip"}}}
	releases[1] = Release{Tag: "v2.0.0", Draft: true, Assets: releases[0].Assets}
	releases[2] = Release{Tag: "v3.0.0", Assets: []Asset{{Name: "binary.zip", URL: "https://cdn.example/binary.zip"}}}
	body, _ := json.Marshal(releases)
	client := &Client{open: func(_ context.Context, address string) (*http.Response, error) {
		if address != "https://api.github.com/repos/owner/site/releases?page=2&per_page=20" {
			t.Fatalf("unexpected query: %s", address)
		}
		return &http.Response{Body: io.NopCloser(bytes.NewReader(body))}, nil
	}}
	page, err := client.Releases(context.Background(), "https://github.com/owner/site", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Releases) != 1 || page.Releases[0].Tag != "v1.0.0" || !page.HasMore || page.Page != 2 {
		t.Fatalf("wrong filtered page: %+v", page)
	}
}

func installationStore(t *testing.T, archive []byte, digest string) (*Store, *int) {
	t.Helper()
	store, err := NewStore(t.TempDir(), []Template{{Path: "user-dist", Name: "Built in"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	store.client = &Client{open: func(_ context.Context, address string) (*http.Response, error) {
		*calls++
		var body []byte
		switch address {
		case "https://api.github.com/repos/owner/site/releases/tags/v1.0.0":
			body, _ = json.Marshal(Release{Tag: "v1.0.0", Name: "Site", Assets: []Asset{{Name: "dist.zip", URL: "https://cdn.example/dist.zip", Size: int64(len(archive)), Digest: digest}}})
		case "https://cdn.example/dist.zip":
			body = archive
		default:
			t.Fatalf("unexpected request %s", address)
		}
		return &http.Response{Body: io.NopCloser(bytes.NewReader(body))}, nil
	}}
	return store, calls
}

func TestInstallPublishesCompleteVersionAndReloadsCatalog(t *testing.T) {
	archive := distributionZIP(t, "dist/index.html", "dist/assets/site.js")
	store, calls := installationStore(t, archive, fmt.Sprintf("sha256:%x", sha256.Sum256(archive)))
	template, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if !template.IsAdmin || !template.Custom || template.IsOfficial || template.Version != "v1.0.0" {
		t.Fatalf("wrong metadata: %+v", template)
	}
	body, err := os.ReadFile(filepath.Join(store.Directory(template.Path), "index.html"))
	if err != nil || !bytes.Contains(body, []byte("installed frontend")) {
		t.Fatalf("installed file missing: %q %v", body, err)
	}
	if len(store.Templates()) != 2 {
		t.Fatal("built-in frontend was replaced")
	}
	reloaded, err := NewStore(store.root, []Template{{Path: "user-dist"}})
	if err != nil || len(reloaded.Templates()) != 2 || reloaded.Directory(template.Path) != store.Directory(template.Path) {
		t.Fatalf("catalog did not survive reload: %v", err)
	}
	duplicate, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", true)
	if err != nil || duplicate.Path != template.Path || *calls != 2 {
		t.Fatalf("install is not idempotent: %+v %v calls=%d", duplicate, err, *calls)
	}
	userTemplate, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false)
	if err != nil || userTemplate.Path == template.Path || userTemplate.IsAdmin {
		t.Fatalf("roles share installation identity: %+v %v", userTemplate, err)
	}
}

func TestFailedInstallNeverPublishesTemplate(t *testing.T) {
	for _, names := range [][]string{{"dist/../../outside"}, {"dist/index.html", "../outside"}, {"index.html"}, {"dist/index.html", "dist/index.html"}, {"dist/index.html", "dist/..\\outside"}, {"dist/assets/site.js"}, {"/dist/index.html"}, {"C:/dist/index.html"}, {"dist/index.html", "dist/assets:stream"}, {"dist/./index.html"}, {"dist//index.html"}} {
		t.Run(strings.Join(names, "_"), func(t *testing.T) {
			store, _ := installationStore(t, distributionZIP(t, names...), "")
			if _, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false); err == nil {
				t.Fatal("unsafe/incomplete archive accepted")
			}
			if len(store.Templates()) != 1 {
				t.Fatal("failed installation became selectable")
			}
			entries, err := os.ReadDir(store.root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed installation left files: %v %v", entries, err)
			}
		})
	}
	store, _ := installationStore(t, distributionZIP(t, "dist/index.html"), "sha256:wrong")
	if _, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch not rejected: %v", err)
	}
	if len(store.Templates()) != 1 {
		t.Fatal("checksum failure published template")
	}
}

func TestDistributionRejectsSymlinks(t *testing.T) {
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	header := &zip.FileHeader{Name: "dist/index.html"}
	header.SetMode(os.ModeSymlink | 0777)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, "../../config.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ := installationStore(t, data.Bytes(), "")
	if _, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestDistributionCannotWriteThroughExistingEscapingSymlink(t *testing.T) {
	destination := t.TempDir()
	outside := t.TempDir()
	canary := filepath.Join(outside, "index.html")
	if err := os.WriteFile(canary, []byte("outside-original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(destination, "dist")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "frontend-distribution.zip")
	if err := os.WriteFile(archive, distributionZIP(t, "dist/assets/site.js", "dist/index.html"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := extractDistribution(context.Background(), archive, destination); err == nil {
		t.Error("extraction accepted a symlink escaping its destination")
	}
	if _, err := os.Stat(filepath.Join(outside, "assets", "site.js")); !os.IsNotExist(err) {
		t.Errorf("extraction wrote through the escaping symlink: %v", err)
	}
	data, err := os.ReadFile(canary)
	if err != nil || string(data) != "outside-original" {
		t.Errorf("outside file changed: %q %v", data, err)
	}
}

func TestRemoveCustomFrontendConfinesFilesAndAllowsReinstall(t *testing.T) {
	store, _ := installationStore(t, distributionZIP(t, "dist/index.html"), "")
	template, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	canary := filepath.Join(outside, "retained.txt")
	if err := os.WriteFile(canary, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.Directory(template.Path), "outside")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"user-dist", "", ".", "..", "../retained.txt", "/etc", `..\outside`, "missing"} {
		if err := store.Remove(id, false); err == nil {
			t.Fatalf("removed invalid or built-in path %q", id)
		}
	}
	if err := store.Remove(template.Path, true); err == nil {
		t.Fatal("removed wrong role")
	}
	if err := store.Remove(template.Path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.root, template.Path)); !os.IsNotExist(err) {
		t.Fatalf("installation remains: %v", err)
	}
	if data, err := os.ReadFile(canary); err != nil || string(data) != "retained" {
		t.Fatalf("outside file changed: %q %v", data, err)
	}
	if len(store.Templates()) != 1 {
		t.Fatalf("catalog still contains installation: %+v", store.Templates())
	}
	reinstalled, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false)
	if err != nil || reinstalled.Path != template.Path {
		t.Fatalf("cannot reinstall: %+v %v", reinstalled, err)
	}
}

func TestRemoveFilesystemFailurePreservesCatalog(t *testing.T) {
	store, _ := installationStore(t, distributionZIP(t, "dist/index.html"), "")
	template, err := store.Install(context.Background(), "https://github.com/owner/site", "v1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the root with a regular file to force a real error even as root.
	saved := store.root + "-unavailable"
	if err := os.Rename(store.root, saved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(saved)
	if err := os.WriteFile(store.root, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(template.Path, false); err == nil {
		t.Fatal("filesystem failure reported as success")
	}
	if len(store.Templates()) != 2 {
		t.Fatal("failed deletion removed catalog entry")
	}
	if _, err := os.Stat(filepath.Join(saved, template.Path, "dist", "index.html")); err != nil {
		t.Fatal(err)
	}
}
