//go:build agentcompat

package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	restoreHTTPSHelperEnv = "NEZHA_RESTORE_HTTPS_HELPER"
	restoreHTTPSCertEnv   = "NEZHA_RESTORE_HTTPS_CERT"
	restoreHTTPSKeyEnv    = "NEZHA_RESTORE_HTTPS_KEY"
)

// With an inherited HTTPS socket, the pre-commit HTTPS check adopts it
// (instead of binding its own port and failing) and the server serves on it
// with the checked certificate.
func TestOpenDashboardHTTPSAdoptsInheritedListener(t *testing.T) {
	certPath, keyPath := writeTestCertificate(t, t.TempDir())
	inherited := newDashboardTCPListener(t, "127.0.0.1:0")
	file, err := inherited.File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	command := exec.Command(os.Args[0], "-test.run=^TestOpenDashboardHTTPSHelper$")
	command.ExtraFiles = []*os.File{file}
	command.Env = append(os.Environ(),
		restoreHTTPSHelperEnv+"=1",
		restoreHTTPSCertEnv+"="+certPath,
		restoreHTTPSKeyEnv+"="+keyPath,
		dashboardHTTPSListenerFDEnv+"=3",
	)
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })

	// The helper prints the address it serves on once it is serving.
	buf := make([]byte, 128)
	n, err := stdout.Read(buf)
	require.NoError(t, err)
	address := strings.TrimSpace(string(buf[:n]))
	require.Equal(t, inherited.Addr().String(), address, "the inherited socket is used")

	pemData, err := os.ReadFile(certPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pemData))
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	resp, err := client.Get("https://" + address)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, "ok", string(body))
}

func TestOpenDashboardHTTPSHelper(t *testing.T) {
	if os.Getenv(restoreHTTPSHelperEnv) != "1" {
		return
	}
	// The configured address is ignored in favour of the inherited socket;
	// binding it would fail.
	l, cert, err := openDashboardHTTPS("127.0.0.1:1", 1, os.Getenv(restoreHTTPSCertEnv), os.Getenv(restoreHTTPSKeyEnv))
	if err != nil {
		t.Fatalf("open https: %v", err)
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ReadHeaderTimeout: time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{*cert}},
	}
	_, _ = os.Stdout.WriteString(l.Addr().String() + "\n")
	_ = serveDashboardHTTPS(server, l)
}
