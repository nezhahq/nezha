// Package frontend manages release archives for the dashboard's static frontends.
package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nezhahq/nezha/pkg/utils"
)

const maxReleaseResponse = 8 << 20

type Release struct {
	Tag         string  `json:"tag_name"`
	Name        string  `json:"name"`
	PublishedAt string  `json:"published_at"`
	Prerelease  bool    `json:"prerelease"`
	Draft       bool    `json:"draft"`
	Assets      []Asset `json:"assets"`
}

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
}

type ReleasePage struct {
	Releases []Release `json:"releases"`
	Page     int       `json:"page"`
	HasMore  bool      `json:"has_more"`
}

// RepositoryAPI accepts repository URLs or an explicit /repos/owner/repo API URL.
// Explicit API URLs also support GitHub Enterprise and compatible self-hosted services.
func RepositoryAPI(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("repository must be an HTTPS repository or release API URL without credentials")
	}
	p := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/releases"), ".git")
	if marker := strings.Index(p, "/repos/"); marker >= 0 {
		parts := strings.Split(strings.TrimPrefix(p[marker:], "/repos/"), "/")
		// Empty path components still split into two parts but do not identify a repository.
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", errors.New("invalid repository API URL")
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", errors.New("repository URL must contain owner and repository")
		}
		switch strings.ToLower(u.Hostname()) {
		case "github.com":
			u.Host = "api.github.com"
			p = "/repos/" + strings.Join(parts, "/")
		case "gitee.com":
			p = "/api/v5/repos/" + strings.Join(parts, "/")
		case "atomgit.com":
			u.Host = "api.atomgit.com"
			p = "/api/v5/repos/" + strings.Join(parts, "/")
		default:
			return "", errors.New("for other hosts, enter the full repository API URL ending in /repos/owner/repository")
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return "", errors.New("invalid repository path")
		}
	}
	u.Path, u.RawPath = p, ""
	return u.String(), nil
}

// openReleaseURL validates and pins every redirect destination separately. Release
// assets redirect to CDNs; following those with the first host's client is unsafe.
func openReleaseURL(ctx context.Context, raw string) (*http.Response, error) {
	for redirects := 0; redirects < 6; redirects++ {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil {
			return nil, errors.New("release downloads must use HTTPS without credentials")
		}
		client, err := utils.NewRestrictedHTTPClient(raw, false)
		if err != nil {
			return nil, err
		}
		client.Timeout = 2 * time.Minute
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", "Nezha-Frontend-Installer")
		response, err := client.Do(request)
		client.CloseIdleConnections()
		if err != nil {
			return nil, err
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location, err := response.Location()
			response.Body.Close()
			if err != nil {
				return nil, err
			}
			raw = location.String()
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("release server returned HTTP %d", response.StatusCode)
		}
		return response, nil
	}
	return nil, errors.New("too many release download redirects")
}

type Client struct {
	open func(context.Context, string) (*http.Response, error)
}

func NewClient() *Client { return &Client{open: openReleaseURL} }

func (c *Client) readJSON(ctx context.Context, address string, target any) error {
	response, err := c.open(ctx, address)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxReleaseResponse {
		return errors.New("release response is too large")
	}
	return json.Unmarshal(data, target)
}

func distributionAsset(release Release) (Asset, bool) {
	if release.Draft {
		return Asset{}, false
	}
	for _, asset := range release.Assets {
		if asset.Name == "dist.zip" && asset.URL != "" && asset.Size >= 0 && asset.Size <= maxArchiveSize {
			return asset, true
		}
	}
	return Asset{}, false
}

func (c *Client) Releases(ctx context.Context, repository string, page int) (*ReleasePage, error) {
	api, err := RepositoryAPI(repository)
	if err != nil {
		return nil, err
	}
	if page < 1 || page > 100 {
		return nil, errors.New("release page must be between 1 and 100")
	}
	var releases []Release
	if err := c.readJSON(ctx, fmt.Sprintf("%s/releases?page=%d&per_page=20", api, page), &releases); err != nil {
		return nil, err
	}
	result := &ReleasePage{Page: page, HasMore: len(releases) == 20, Releases: []Release{}}
	for _, release := range releases {
		if asset, ok := distributionAsset(release); ok && release.Tag != "" {
			release.Assets = []Asset{asset}
			result.Releases = append(result.Releases, release)
		}
	}
	return result, nil
}

func (c *Client) release(ctx context.Context, repository, tag string) (Release, error) {
	api, err := RepositoryAPI(repository)
	if err != nil {
		return Release{}, err
	}
	if tag == "" || len(tag) > 200 {
		return Release{}, errors.New("invalid release tag")
	}
	var release Release
	err = c.readJSON(ctx, api+"/releases/tags/"+url.PathEscape(tag), &release)
	if err == nil && release.Tag != tag {
		err = errors.New("release server returned a different tag")
	}
	return release, err
}
