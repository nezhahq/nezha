//go:build !windows

package script

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The stand-in wget records where it was asked to write and, like the real
// one, creates the output file before it knows whether the download works.
const stubWget = `#!/bin/sh
printf '%s' "$2" >"$STUB_DIR/output_path"
ls -ld "$(dirname "$2")" | cut -c1-10 >"$STUB_DIR/output_dir_mode"
: >"$2" || exit 1
[ -z "$STUB_WGET_FAIL" ] || exit 4
printf '#!/bin/sh\necho "downloaded installer ran: $*"\n' >"$2"
`

// install.sh probes its location with curl; keep that offline.
const stubCurl = `#!/bin/sh
exit 0
`

func runInstallScript(t *testing.T, script string, failDownload bool) (workDir, tmpDir, stubDir, output string, err error) {
	t.Helper()

	scriptPath, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}

	workDir, tmpDir, stubDir = t.TempDir(), t.TempDir(), t.TempDir()
	for name, content := range map[string]string{"wget": stubWget, "curl": stubCurl} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("sh", scriptPath, "--flag", "value")
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader("\n")
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+tmpDir,
		"STUB_DIR="+stubDir,
	)
	if failDownload {
		cmd.Env = append(cmd.Env, "STUB_WGET_FAIL=1")
	}

	out, err := cmd.CombinedOutput()
	return workDir, tmpDir, stubDir, string(out), err
}

func TestInstallScriptsDownloadToPrivateDirectory(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		t.Run(script, func(t *testing.T) {
			workDir, tmpDir, stubDir, output, err := runInstallScript(t, script, false)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, output)
			}

			outputPath, err := os.ReadFile(filepath.Join(stubDir, "output_path"))
			if err != nil {
				t.Fatal(err)
			}
			// A fixed name in a shared directory can be planted by another local user.
			downloadDir := filepath.Dir(string(outputPath))
			if filepath.Dir(downloadDir) != tmpDir {
				t.Fatalf("download was written to %q, want a new directory under %q", outputPath, tmpDir)
			}
			mode, err := os.ReadFile(filepath.Join(stubDir, "output_dir_mode"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(mode)); got != "drwx------" {
				t.Fatalf("download directory mode = %s, want drwx------", got)
			}

			if !strings.Contains(output, "downloaded installer ran: --flag value") {
				t.Fatalf("downloaded installer was not run with the original arguments:\n%s", output)
			}
			if _, err := os.Stat(filepath.Join(workDir, "nezha.sh")); err != nil {
				t.Fatalf("downloaded installer was not moved into the working directory: %v", err)
			}
			if _, err := os.Stat(downloadDir); !os.IsNotExist(err) {
				t.Fatalf("download directory %q was left behind (err=%v)", downloadDir, err)
			}
		})
	}
}

func TestInstallScriptsAbortOnFailedDownload(t *testing.T) {
	for _, script := range []string{"install.sh", "install_en.sh"} {
		t.Run(script, func(t *testing.T) {
			workDir, tmpDir, _, output, err := runInstallScript(t, script, true)
			if err == nil {
				t.Fatalf("installer succeeded after a failed download:\n%s", output)
			}

			if _, err := os.Stat(filepath.Join(workDir, "nezha.sh")); !os.IsNotExist(err) {
				t.Fatalf("a failed download was moved into the working directory (err=%v)", err)
			}
			entries, err := os.ReadDir(tmpDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("download directory was left behind in %q", tmpDir)
			}
		})
	}
}
