package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/nezhahq/nezha/model"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// killSwitchStream is a minimal RequestTask stream that just records sent
// tasks; it never replies. CallAgent under this stream blocks until the
// kill switch wakes it up, which is exactly the behaviour these tests
// pin down.
type killSwitchStream struct {
	sent chan *pb.Task
}

func newKillSwitchStream() *killSwitchStream {
	return &killSwitchStream{sent: make(chan *pb.Task, 4)}
}

func (s *killSwitchStream) Send(t *pb.Task) error         { s.sent <- t; return nil }
func (s *killSwitchStream) Recv() (*pb.TaskResult, error) { return nil, context.Canceled }
func (s *killSwitchStream) SetHeader(metadata.MD) error   { return nil }
func (s *killSwitchStream) SendHeader(metadata.MD) error  { return nil }
func (s *killSwitchStream) SetTrailer(metadata.MD)        {}
func (s *killSwitchStream) Context() context.Context      { return context.Background() }
func (s *killSwitchStream) SendMsg(any) error             { return nil }
func (s *killSwitchStream) RecvMsg(any) error             { return context.Canceled }

func TestRevalidateTransferEntry_BlocksWhenMCPDisabled(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	singleton.Conf.SetMCPEnabled(false)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	entry := &transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/file",
		Direction: transferDirDownload,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}

	err := revalidateTransferEntry(entry)
	require.Error(t, err, "revalidate must reject when EnableMCP=false")
	require.Contains(t, err.Error(), "MCP is disabled",
		"error message must surface kill switch reason, not look like a transient agent fault")
}

func TestPurgeTransferEntries_DropsMintedTokens(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	for i := 0; i < 3; i++ {
		_, err := mintTransferToken(transferEntry{
			UserID:    uid,
			TokenID:   tok.ID,
			ServerID:  7,
			Path:      "/srv/blob",
			Direction: transferDirDownload,
			ExpiresAt: time.Now().Add(5 * time.Minute),
		})
		require.NoError(t, err)
	}

	purged := PurgeTransferEntries()
	require.GreaterOrEqual(t, purged, 3, "all minted entries must be dropped")

	count := 0
	transferEntries.Range(func(_, _ any) bool { count++; return true })
	require.Equal(t, 0, count, "transferEntries must be empty after purge")
}

func TestRevokeStreamsForPurpose_OnlyTouchesMatchingPurpose(t *testing.T) {
	h := rpc.NewNezhaHandler()
	h.CreateStreamWithPurpose("legacy-1", 0, 1, rpc.PurposeLegacy)
	h.CreateStreamWithPurpose("mcp-1", 0, 1, rpc.PurposeMCPTransfer)
	h.CreateStreamWithPurpose("mcp-2", 0, 2, rpc.PurposeMCPTransfer)

	revoked := h.RevokeStreamsForPurpose(rpc.PurposeMCPTransfer)
	require.Equal(t, 2, revoked, "kill switch must take down both MCP streams")

	_, legacyErr := h.GetStream("legacy-1")
	require.NoError(t, legacyErr,
		"legacy purpose streams (terminal/fm/nat) must NOT be revoked by the MCP kill switch")
	_, mcp1Err := h.GetStream("mcp-1")
	require.Error(t, mcp1Err, "mcp-1 must be gone after revoke")
	_, mcp2Err := h.GetStream("mcp-2")
	require.Error(t, mcp2Err, "mcp-2 must be gone after revoke")
}

func TestCancelAllMCPInflight_UnblocksCallAgent(t *testing.T) {
	stream := newKillSwitchStream()
	original := singleton.ServerShared
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = 88
	srv.SetTaskStream(stream)
	sc.InsertForTest(srv)
	singleton.ServerShared = sc
	t.Cleanup(func() { singleton.ServerShared = original })

	done := make(chan error, 1)
	go func() {
		_, err := rpc.CallAgent(context.Background(), 88, model.TaskTypeExec,
			model.ExecRequest{Cmd: "sleep"}, 30*time.Second)
		done <- err
	}()

	select {
	case <-stream.sent:
	case <-time.After(time.Second):
		t.Fatalf("CallAgent never reached stream.Send within 1s")
	}

	rpc.CancelAllMCPInflight()

	select {
	case err := <-done:
		require.ErrorIs(t, err, rpc.ErrMCPDisabled,
			"CallAgent must surface ErrMCPDisabled when kill switch fires; got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("CallAgent did not return after CancelAllMCPInflight; kill switch is broken")
	}
}

func TestUpdateConfig_DisablingMCPInvokesKillSwitch(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	installTestConfig(t)
	singleton.Conf.SetMCPEnabled(true)

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	_, err := mintTransferToken(transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/blob",
		Direction: transferDirDownload,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	require.NoError(t, err)

	originalHandler := rpc.NezhaHandlerSingleton
	rpc.NezhaHandlerSingleton = rpc.NewNezhaHandler()
	t.Cleanup(func() { rpc.NezhaHandlerSingleton = originalHandler })
	rpc.NezhaHandlerSingleton.CreateStreamWithPurpose("mcp-active", 0, 7, rpc.PurposeMCPTransfer)

	stream := newKillSwitchStream()
	sc := singleton.NewEmptyServerClassForTest()
	srv := &model.Server{}
	srv.ID = 7
	srv.SetTaskStream(stream)
	sc.InsertForTest(srv)
	originalShared := singleton.ServerShared
	singleton.ServerShared = sc
	t.Cleanup(func() { singleton.ServerShared = originalShared })

	rpcDone := make(chan error, 1)
	go func() {
		_, err := rpc.CallAgent(context.Background(), 7, model.TaskTypeFsRead,
			model.FsReadRequest{Path: "/x"}, 30*time.Second)
		rpcDone <- err
	}()
	select {
	case <-stream.sent:
	case <-time.After(time.Second):
		t.Fatalf("background CallAgent never reached the stream")
	}

	origTemplates := singleton.FrontendTemplates
	singleton.FrontendTemplates = []model.FrontendTemplate{{Path: "user-dist", IsAdmin: false}}
	defer func() { singleton.FrontendTemplates = origTemplates }()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setAuthUser(c, uid, model.RoleAdmin)
		c.Next()
	})
	r.PATCH("/api/v1/setting", commonHandler(updateConfig))
	settingBody := map[string]any{
		"site_name":     "test",
		"language":      "en_US",
		"user_template": "user-dist",
		"enable_mcp":    false,
	}
	raw, _ := json.Marshal(settingBody)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/setting", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	success, errMsg := decodeCommonResponseError(t, w.Body.Bytes())
	require.True(t, success, "PATCH /setting must succeed: %s", errMsg)
	require.False(t, singleton.Conf.EnableMCP, "config must reflect kill switch state")

	count := 0
	transferEntries.Range(func(_, _ any) bool { count++; return true })
	require.Equal(t, 0, count, "unconsumed transfer URLs must be purged")

	_, streamErr := rpc.NezhaHandlerSingleton.GetStream("mcp-active")
	require.Error(t, streamErr, "active MCP IOStream must be revoked")

	select {
	case err := <-rpcDone:
		require.True(t, errors.Is(err, rpc.ErrMCPDisabled),
			"in-flight CallAgent must wake up with ErrMCPDisabled, got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("in-flight CallAgent did not wake up after kill switch")
	}
}

// A one-time transfer URL minted with an API token must not outlive the
// owner's credential change any more than the token itself does.
func TestRevalidateTransferEntry_BlocksAfterOwnerCredentialChange(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()

	tok, _ := mkToken(t, uid, []string{model.ScopeServerRead}, nil)
	entry := &transferEntry{
		UserID:    uid,
		TokenID:   tok.ID,
		ServerID:  7,
		Path:      "/srv/file",
		Direction: transferDirDownload,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	require.NoError(t, revalidateTransferEntry(entry))

	// what updateProfile does
	require.NoError(t, singleton.DB.Model(&model.User{}).Where("id = ?", uid).Update("token_version", 1).Error)

	require.ErrorContains(t, revalidateTransferEntry(entry), "revoked by credential change")
}
