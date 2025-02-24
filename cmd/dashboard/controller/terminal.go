package controller

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-uuid"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/websocketx"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

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
	var createTerminalReq model.TerminalForm
	if err := c.ShouldBind(&createTerminalReq); err != nil {
		return nil, err
	}

	server, _ := singleton.ServerShared.Get(createTerminalReq.ServerID)
	if server == nil || server.TaskStream == nil {
		return nil, singleton.Localizer.ErrorT("server not found or not connected")
	}

	if !server.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}

	rpc.NezhaHandlerSingleton.CreateStream(streamId)

	terminalData, _ := json.Marshal(&model.TerminalTask{
		StreamID: streamId,
	})
	if err := server.TaskStream.Send(&proto.Task{
		Type: model.TaskTypeTerminalGRPC,
		Data: string(terminalData),
	}); err != nil {
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
	if _, err := rpc.NezhaHandlerSingleton.GetStream(streamId); err != nil {
		return nil, err
	}
	defer rpc.NezhaHandlerSingleton.CloseStream(streamId)

	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return nil, newWsError("%v", err)
	}
	defer wsConn.Close()
	conn := websocketx.NewConn(wsConn)

	go func() {
		// PING 保活
		for {
			if err = conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
			time.Sleep(time.Second * 10)
		}
	}()

	if err = rpc.NezhaHandlerSingleton.UserConnected(streamId, conn); err != nil {
		return nil, newWsError("%v", err)
	}

	if err = rpc.NezhaHandlerSingleton.StartStream(streamId, time.Second*10); err != nil {
		return nil, newWsError("%v", err)
	}

	return nil, newWsError("")
}
