package controller

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/hashicorp/go-uuid"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/websocketx"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

// The official file-manager client uploads in 1 MiB WebSocket messages. Cap
// each complete message at that protocol boundary so gorilla/websocket cannot
// buffer an attacker-sized frame before the IO stream relay sees it.
const fileManagerWebSocketInputLimit int64 = 1024 * 1024

type websocketReadLimiter interface {
	SetReadLimit(limit int64)
}

func limitFileManagerWebSocketInput(conn websocketReadLimiter) {
	conn.SetReadLimit(fileManagerWebSocketInputLimit)
}

// Create FM session
// @Summary Create FM session
// @Description Create an "attached" FM. It is advised to only call this within a terminal session.
// @Tags auth required
// @Accept json
// @Param id query uint true "Server ID"
// @Produce json
// @Success 200 {object} model.CreateFMResponse
// @Router /file [post]
func createFM(c *gin.Context) (*model.CreateFMResponse, error) {
	prepareAgentcompatCapabilityHeader(c)
	idStr := c.Query("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, _ := singleton.ServerShared.Get(id)
	if server == nil {
		return nil, singleton.Localizer.ErrorT("server not found or not connected")
	}
	if server.GetTaskStream() == nil {
		return nil, singleton.Localizer.ErrorT("server not found or not connected")
	}

	if !server.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	releaseControl, controlErr := beginServerControl(c, server.ID)
	if controlErr != nil {
		return nil, controlErr
	}
	defer releaseControl()

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}

	cleanup, err := createIOStreamWithAgentcompatCapability(c, streamId, getUid(c), server.ID, rpc.AgentCompatCapabilityFileManager)
	if err != nil {
		return nil, err
	}

	fmData, err := json.Marshal(&model.TaskFM{
		StreamID: streamId,
	})
	if err != nil {
		// A stream is owned by the caller only after this function succeeds.
		cleanup()
		return nil, err
	}
	if err := server.SendTask(&proto.Task{
		Type: model.TaskTypeFM,
		Data: string(fmData),
	}); err != nil {
		cleanup()
		return nil, err
	}

	return &model.CreateFMResponse{
		SessionID: streamId,
	}, nil
}

// Start FM stream
// @Summary Start FM stream
// @Description Start FM stream
// @Tags auth required
// @Param id path string true "Stream UUID"
// @Success 200 {object} model.CommonResponse[any]
// @Router /ws/file/{id} [get]
func fmStream(c *gin.Context) (any, error) {
	streamId := c.Param("id")
	// GHSA-style fix: io_stream sessions must be reachable only by their creator
	// (or an admin). Without this, any authenticated user who learns a stream
	// UUID can hijack a live file-manager session on the target server.
	if !streamAttachAllowedForRequest(c, streamId) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}
	if _, err := rpc.NezhaHandlerSingleton.GetStream(streamId); err != nil {
		return nil, err
	}
	defer rpc.NezhaHandlerSingleton.CloseStream(streamId)

	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return nil, newWsError("%v", err)
	}
	limitFileManagerWebSocketInput(wsConn)
	conn := websocketx.NewConn(wsConn)
	pingTransport := newWebsocketPingTransport(conn, wsConn.Close)
	stopPing := startWebsocketPingTicker(c.Request.Context(), time.Second*10, pingTransport)

	deregisterPAT := registerPATConnection(c, func() { _ = pingTransport.Close() })
	defer deregisterPAT()
	// Join the ping worker before PAT and WebSocket cleanup can close its writer.
	defer stopPing()

	if err = rpc.NezhaHandlerSingleton.UserConnected(streamId, conn); err != nil {
		return nil, newWsError("%v", err)
	}

	if err = rpc.NezhaHandlerSingleton.StartStream(streamId, time.Second*10); err != nil {
		return nil, newWsError("%v", err)
	}

	return nil, newWsError("")
}
