package controller

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
	"github.com/stretchr/testify/require"
)

func TestFrontendPathConfinementForEmbeddedAndLocalOverrides(t *testing.T) {
	for _, mode := range []string{"embedded", "local-override"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			original := singleton.Conf
			singleton.Conf = &singleton.ConfigClass{Config: &model.Config{ConfigDashboard: model.ConfigDashboard{UserTemplate: "user-dist", AdminTemplate: "admin-dist"}}}
			t.Cleanup(func() { singleton.Conf = original })
			files := fstest.MapFS{
				"user-dist/index.html":             &fstest.MapFile{Data: []byte("embedded user index")},
				"user-dist/assets/application.js":  &fstest.MapFile{Data: []byte("embedded user asset")},
				"admin-dist/index.html":            &fstest.MapFile{Data: []byte("embedded admin index")},
				"admin-dist/assets/application.js": &fstest.MapFile{Data: []byte("embedded admin asset")},
				"data/config.yaml":                 &fstest.MapFile{Data: []byte("outside-secret-canary")},
				"other-dist/private.txt":           &fstest.MapFile{Data: []byte("other-theme-canary")},
			}
			// Distinct contents prove local files are served instead of silently falling back to embedded files.
			expectedSource := "embedded "
			if mode == "local-override" {
				expectedSource = "local "
				for name, file := range files {
					require.NoError(t, os.MkdirAll(filepath.Dir(name), 0750))
					require.NoError(t, os.WriteFile(name, []byte(strings.ReplaceAll(string(file.Data), "embedded ", "local ")), 0600))
				}
				outside, err := filepath.Abs("data/config.yaml")
				require.NoError(t, err)
				for _, directory := range []string{"user-dist", "admin-dist"} {
					require.NoError(t, os.Symlink(outside, filepath.Join(directory, "outside-link.txt")))
				}
			}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.NoRoute(fallbackToFrontend(files))
			for _, role := range []struct{ prefix, name string }{{"/", "user"}, {"/dashboard/", "admin"}} {
				valid := performFrontendFallbackRequest(t, router, role.prefix+"assets/application.js")
				require.Equal(t, http.StatusOK, valid.Code)
				require.Equal(t, expectedSource+role.name+" asset", valid.Body.String())
				attacks := []string{"../data/config.yaml", "../../data/config.yaml", "%2e%2e/data/config.yaml", "%2e%2e%2fdata/config.yaml", "%252e%252e/data/config.yaml", "..%5cdata%5cconfig.yaml", "../other-dist/private.txt", "../admin-dist/assets/application.js", "../user-dist/assets/application.js", "/etc/passwd", "C:/Windows/win.ini"}
				if mode == "local-override" {
					outside, err := filepath.Abs("data/config.yaml")
					require.NoError(t, err)
					attacks = append(attacks, "outside-link.txt", filepath.ToSlash(outside))
				}
				for _, attack := range attacks {
					response := performFrontendFallbackRequest(t, router, role.prefix+attack)
					require.Equal(t, http.StatusNotFound, response.Code, role.prefix+attack)
					require.Equal(t, expectedSource+role.name+" index", response.Body.String(), role.prefix+attack)
				}
			}
			for _, attack := range []string{"/dashboard../data/config.yaml", "/dashboard%2e%2e/data/config.yaml", "/dashboard%2e%2e%2fdata/config.yaml"} {
				response := performFrontendFallbackRequest(t, router, attack)
				require.Equal(t, http.StatusNotFound, response.Code, attack)
				require.Equal(t, expectedSource+"user index", response.Body.String(), attack)
			}
		})
	}
}
