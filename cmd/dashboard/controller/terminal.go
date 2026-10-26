package controller

import (
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

// Allow the frontend's 512 KiB clipboard payload plus xterm's bracketed-paste
// control bytes, while keeping the complete tagged message below the 1 MiB
// IO stream relay buffer.
const terminalWebSocketInputLimit int64 = 512*1024 + 64

// Create web ssh terminal
// @Summary Create web ssh terminal
// @Description Create web ssh terminal
// @Tags auth required
// @Accept json
// @Param terminal body model.TerminalForm true "TerminalForm"
// @Produce json
// @Success 200 {object} model.CreateTerminalResponse
// @Router /terminal [post]
func createTerminal(c *gin.Context) (*model.CreateTerminalResponse, error) {
	prepareAgentcompatCapabilityHeader(c)
	var createTerminalReq model.TerminalForm
	if err := c.ShouldBind(&createTerminalReq); err != nil {
		return nil, err
	}

	server, _ := singleton.ServerShared.Get(createTerminalReq.ServerID)
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

	cleanup, err := createIOStreamWithAgentcompatCapability(c, streamId, getUid(c), server.ID, rpc.AgentCompatCapabilityTerminal)
	if err != nil {
		return nil, err
	}

	terminalData, err := json.Marshal(&model.TerminalTask{
		StreamID: streamId,
	})
	if err != nil {
		// A stream is owned by the caller only after this function succeeds.
		cleanup()
		return nil, err
	}
	if err := server.SendTask(&proto.Task{
		Type: model.TaskTypeTerminalGRPC,
		Data: string(terminalData),
	}); err != nil {
		cleanup()
		return nil, err
	}

	return &model.CreateTerminalResponse{
		SessionID:  streamId,
		ServerID:   server.ID,
		ServerName: server.Name,
	}, nil
}

// TerminalStream web ssh terminal stream
// @Summary Terminal stream
// @Description Terminal stream
// @Tags auth required
// @Param id path string true "Stream UUID"
// @Success 200 {object} model.CommonResponse[any]
// @Router /ws/terminal/{id} [get]
func terminalStream(c *gin.Context) (any, error) {
	streamId := c.Param("id")
	// GHSA-style fix: io_stream sessions must be reachable only by their creator
	// (or an admin). Without this, any authenticated user who learns a stream
	// UUID — via Referer leak, access logs, browser history — can hijack a live
	// terminal and gain shell access to the target server.
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
	wsConn.SetReadLimit(terminalWebSocketInputLimit)
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
