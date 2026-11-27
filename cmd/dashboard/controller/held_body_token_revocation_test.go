package controller

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// heldRequestBody delivers its data only after release is closed, like a
// client that sends the request head now and the body later.
type heldRequestBody struct {
	reader  *bytes.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldRequestBody(data []byte) *heldRequestBody {
	return &heldRequestBody{reader: bytes.NewReader(data), started: make(chan struct{}), release: make(chan struct{})}
}

func (b *heldRequestBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.reader.Read(p)
}

func (b *heldRequestBody) Close() error { return nil }

// A password/profile change revokes the user's API tokens. A REST request
// whose head was authenticated before the change must not run once its body
// arrives afterwards: the credential has to be checked with the whole
// request in hand.
func TestRESTRequestWithHeldBodyFailsAfterTokenRevocation(t *testing.T) {
	env := newBackupTestEnv(t)
	require.NoError(t, singleton.DB.AutoMigrate(&model.ServerGroup{}, &model.ServerGroupServer{}))

	body := newHeldRequestBody([]byte(`{"name":"created-by-revoked-token","servers":[]}`))
	req := httptest.NewRequest(http.MethodPost, "https://example.com/api/v1/server-group", nil)
	req.Body = body
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.pat)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.router.ServeHTTP(rec, req)
	}()

	select {
	case <-body.started:
	case <-done:
		t.Fatalf("request finished before reading its body: %d %s", rec.Code, rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("the body was never read")
	}
	// The owner changes the password while the body is still on its way.
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", 1).
		Update("token_version", gorm.Expr("token_version + 1")).Error)
	close(body.release)
	<-done

	var groups int64
	require.NoError(t, singleton.DB.Model(&model.ServerGroup{}).Count(&groups).Error)
	require.Zero(t, groups, "the revoked token still created a server group: %d %s", rec.Code, rec.Body.String())
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	_, _ = io.Copy(io.Discard, rec.Body)
}

// The MCP route re-checks the credential when the body arrives; that check
// must also refuse a token revoked by a password/profile change, after the
// short-lived revocation tombstone is gone.
func TestMCPCallRecheckRefusesTokenRevokedByCredentialChange(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeNezhaAll}, nil)
	c, _ := mcpCallCtx(t, tok, uid, map[string]any{})
	require.NoError(t, revalidateMCPCallCredential(c, tok))

	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", uid).
		Update("token_version", gorm.Expr("token_version + 1")).Error)
	require.ErrorIs(t, revalidateMCPCallCredential(c, tok), errMCPCredentialInvalid)
}

// Reading the body before authentication must not let a client keep a
// connection (and up to the body limit of memory) forever: the dashboard has
// no read timeout once the head is read, so the body read has its own.
func TestRESTRequestBodyReadHasADeadline(t *testing.T) {
	env := newBackupTestEnv(t)
	require.NoError(t, singleton.DB.AutoMigrate(&model.ServerGroup{}, &model.ServerGroupServer{}))
	original := restRequestBodyReadTimeout
	restRequestBodyReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { restRequestBodyReadTimeout = original })

	server := httptest.NewServer(env.router)
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	body := `{"name":"held-past-the-deadline","servers":[]}`
	_, err = fmt.Fprintf(conn, "POST /api/v1/server-group HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", env.pat, len(body), body[:1])
	require.NoError(t, err)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the dashboard kept waiting for the body")
	defer response.Body.Close()
	require.Equal(t, http.StatusRequestTimeout, response.StatusCode)

	// The rest of the body arrives too late: nothing is created.
	_, _ = conn.Write([]byte(body[1:]))
	var groups int64
	require.NoError(t, singleton.DB.Model(&model.ServerGroup{}).Count(&groups).Error)
	require.Zero(t, groups)
}
