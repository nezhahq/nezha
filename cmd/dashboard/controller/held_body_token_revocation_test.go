package controller

import (
	"bytes"
	"io"
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
