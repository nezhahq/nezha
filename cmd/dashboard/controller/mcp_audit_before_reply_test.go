package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/nezhahq/nezha/model"
)

// The client decides how long a response body takes and whether its last byte
// is ever read, so an audit row that waits for the body can be withheld by the
// client. These tests pin the order: recorded first, then answered.

// stalledClientWriter stands for a client that takes no byte of the body.
type stalledClientWriter struct {
	*countingDiscardWriter
	onWrite func()
}

func (w *stalledClientWriter) Write([]byte) (int, error) {
	w.onWrite()
	return 0, errors.New("client disconnected")
}

func TestRelayDownloadFrames_RecordsBeforeBodyIsWritten(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 64<<10)
	var src bytes.Buffer
	writeChunkFrame(&src, payload)
	writeOKFrame(&src, uint64(len(payload)))

	recorded := 0
	client := &stalledClientWriter{countingDiscardWriter: newCountingDiscardWriter()}
	client.onWrite = func() {
		require.Equal(t, 1, recorded, "body bytes were offered to the client before the download was recorded")
	}
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(client)

	err := relayDownloadFrames(context.Background(), c, &fixedSizeFrameStream{buf: src}, int64(len(payload)), func() { recorded++ })
	require.Error(t, err, "the client write error must still be returned")
	require.Equal(t, 1, recorded)
}

// A payload the agent did not complete is not a download: beforeBody stays uncalled.
func TestRelayDownloadFrames_DoesNotRecordIncompletePayload(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 1<<10)
	var src bytes.Buffer
	writeChunkFrame(&src, payload)
	writeOKFrame(&src, uint64(len(payload))+1)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	err := relayDownloadFrames(context.Background(), c, &fixedSizeFrameStream{buf: src}, int64(len(payload)), func() {
		t.Error("an unverified payload was recorded as a download")
	})
	require.Error(t, err)
}

// transferTokenParam returns the :token route parameter of a minted transfer URL.
func transferTokenParam(url string) gin.Params {
	return gin.Params{{Key: "token", Value: url[strings.LastIndex(url, "/")+1:]}}
}

// A client that hangs up with the body unread has still been served the file:
// the row is "ok" and exists before the first body byte, not "agent_error".
func TestTransferDownload_AuditsOKBeforeBodyIsWritten(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 64<<10)
	ts, tok, cleanup := setupTransferTest(t, xferAgentDownloadSend(payload))
	defer cleanup()
	url := mintDownloadURL(t, ts, tok, "/srv/blob")

	writes := 0
	client := &stalledClientWriter{countingDiscardWriter: newCountingDiscardWriter()}
	client.onWrite = func() {
		writes++
		require.Equal(t, int64(1), countAuditRows(t, "fs.download", model.MCPOutcomeOK),
			"the file was offered to the client before the download was audited")
	}
	c, _ := gin.CreateTestContext(client)
	c.Request = httptest.NewRequest(http.MethodGet, url, nil)
	c.Params = transferTokenParam(url)
	transferDownloadHandler(c)

	require.NotZero(t, writes)
	require.Equal(t, int64(1), countAuditRows(t, "fs.download", ""),
		"a client that stops reading must not turn the download into an agent_error row")
}

// writeObservingWriter calls onWrite before each body write of a gin response.
type writeObservingWriter struct {
	gin.ResponseWriter
	onWrite func()
}

func (w *writeObservingWriter) Write(p []byte) (int, error) {
	w.onWrite()
	return w.ResponseWriter.Write(p)
}

func TestTransferUpload_AuditsOKBeforeReplyIsWritten(t *testing.T) {
	ts, tok, cleanup := setupTransferTest(t, xferAgentUploadAccept)
	defer cleanup()
	url := mintUploadURL(t, ts, tok, "/srv/upload.bin")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writes := 0
	c.Writer = &writeObservingWriter{ResponseWriter: c.Writer, onWrite: func() {
		writes++
		require.Equal(t, int64(1), countAuditRows(t, "fs.upload", model.MCPOutcomeOK),
			"the upload was acknowledged to the client before it was audited")
	}}
	c.Request = httptest.NewRequest(http.MethodPost, url, strings.NewReader("payload"))
	c.Params = transferTokenParam(url)
	transferUploadHandler(c)

	require.NotZero(t, writes)
	require.Equal(t, http.StatusOK, w.Code, "body=%q", w.Body.String())
}

func TestMCPToolsCall_AuditsBeforeAnswering(t *testing.T) {
	cleanup, uid := setupMCPTest(t)
	defer cleanup()
	tok, _ := mkToken(t, uid, []string{model.ScopeInventoryRead}, nil)

	c, w := mcpCallCtx(t, tok, uid, nil)
	writes := 0
	c.Writer = &writeObservingWriter{ResponseWriter: c.Writer, onWrite: func() {
		writes++
		require.Equal(t, int64(1), countAuditRows(t, "server.list", model.MCPOutcomeOK),
			"the tool result was offered to the client before the call was audited")
	}}

	params, err := json.Marshal(toolCallParams{Name: "server.list", Arguments: json.RawMessage(`{}`)})
	require.NoError(t, err)
	handleToolsCall(c, &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call", Params: params}, tok)

	require.NotZero(t, writes)
	_, result := decodeRPC(w)
	require.NotNil(t, result)
	require.False(t, result.IsError)
}
