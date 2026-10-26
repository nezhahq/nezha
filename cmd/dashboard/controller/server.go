package controller

import (
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// List server
// @Summary List server
// @Security BearerAuth
// @Security APITokenAuth
// @Schemes
// @Description List server. PAT scope required: nezha:inventory:read.
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Server]
// @Router /server [get]
func listServer(c *gin.Context) ([]*model.Server, error) {
	slist := singleton.ServerShared.GetSortedList()
	ssl := make([]*model.Server, 0, len(slist))
	for _, server := range slist {
		if server == nil {
			continue
		}
		runtime := server.RuntimeSnapshot()
		ssl = append(ssl, server.RuntimeCopy(runtime))
	}
	return ssl, nil
}

// Edit server
// @Summary Edit server
// @Security BearerAuth
// @Schemes
// @Description Edit server
// @Tags auth required
// @Accept json
// @Param id path uint true "Server ID"
// @Param body body model.ServerForm true "ServerForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /server/{id} [patch]
func updateServer(c *gin.Context) (any, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}
	var sf model.ServerForm
	if err := c.ShouldBindJSON(&sf); err != nil {
		return nil, err
	}

	if !singleton.DDNSShared.CheckPermission(c, slices.Values(sf.DDNSProfiles)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	var s model.Server
	if err := singleton.DB.First(&s, id).Error; err != nil {
		return nil, singleton.Localizer.ErrorT("server id %d does not exist", id)
	}

	if !s.HasPermission(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	s.Name = sf.Name
	s.DisplayIndex = sf.DisplayIndex
	s.Note = sf.Note
	s.PublicNote = sf.PublicNote
	s.HideForGuest = sf.HideForGuest
	s.EnableDDNS = sf.EnableDDNS
	s.DDNSProfiles = sf.DDNSProfiles
	s.OverrideDDNSDomains = sf.OverrideDDNSDomains

	ddnsProfilesRaw, err := json.Marshal(s.DDNSProfiles)
	if err != nil {
		return nil, err
	}
	s.DDNSProfilesRaw = string(ddnsProfilesRaw)

	overrideDomainsRaw, err := json.Marshal(sf.OverrideDDNSDomains)
	if err != nil {
		return nil, err
	}
	s.OverrideDDNSDomainsRaw = string(overrideDomainsRaw)

	if err := singleton.DB.Save(&s).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	rs, _ := singleton.ServerShared.Get(s.ID)
	s.CopyFromRunningServer(rs)
	singleton.ServerShared.Update(&s, "")

	return nil, nil
}

// Batch delete server
// @Summary Batch delete server
// @Security BearerAuth
// @Schemes
// @Description Batch delete server
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/server [post]
func batchDeleteServer(c *gin.Context) (any, error) {
	var servers []uint64
	if err := c.ShouldBindJSON(&servers); err != nil {
		return nil, err
	}

	if !singleton.ServerShared.CheckPermission(c, slices.Values(servers)) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	err := singleton.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Delete(&model.Server{}, "id in (?)", servers).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Delete(&model.ServerGroupServer{}, "server_id in (?)", servers).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.AlertsLock.Lock()
	for _, sid := range servers {
		for _, alert := range singleton.Alerts {
			if singleton.AlertsCycleTransferStatsStore[alert.ID] != nil {
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].ServerName, sid)
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].Transfer, sid)
				delete(singleton.AlertsCycleTransferStatsStore[alert.ID].NextUpdate, sid)
			}
		}
	}
	singleton.DB.Unscoped().Delete(&model.Transfer{}, "server_id in (?)", servers)
	singleton.AlertsLock.Unlock()

	// Cancel any in-flight transfers BEFORE the in-memory ServerShared
	// entry is dropped: the order shortens the window in which a
	// concurrent Retry/Register could install a fresh pending entry for
	// the same serverID and have it wiped by the cleanup. The
	// transferID-guarded delete inside OnServersDeleted is the
	// authoritative protection against that race; the ordering here is
	// belt and braces.
	singleton.ServerTransferShared.OnServersDeleted(servers)
	singleton.ServerShared.Delete(servers)
	return nil, nil
}

// Force update Agent
// @Summary Force update Agent
// @Security BearerAuth
// @Schemes
// @Description Force update Agent
// @Tags auth required
// @Accept json
// @param request body []uint64 true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServerTaskResponse]
// @Router /force-update/server [post]
func forceUpdateServer(c *gin.Context) (*model.ServerTaskResponse, error) {
	var forceUpdateServers []uint64
	if err := c.ShouldBindJSON(&forceUpdateServers); err != nil {
		return nil, err
	}

	forceUpdateResp := new(model.ServerTaskResponse)

	for _, sid := range forceUpdateServers {
		server, _ := singleton.ServerShared.Get(sid)
		// Per-ID ownership check. Foreign servers (online or offline) and
		// unknown IDs MUST be indistinguishable in the response — otherwise the
		// response shape leaks server-ID existence/online-state, letting a
		// RoleMember enumerate other users' machines. We drop them into the
		// Offline bucket without actually dispatching the upgrade task.
		if server == nil || !server.HasPermission(c) {
			forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
			continue
		}
		if server.GetTaskStream() != nil {
			// Upgrade is a control operation too; recheck ownership and pending
			// transitions at the actual send, including timeout cleanup.
			if err := server.SendTaskGuarded(&pb.Task{
				Type: model.TaskTypeUpgrade,
			}, serverTaskAdmission(c, sid)); err != nil {
				if errors.Is(err, model.ErrTaskStreamOffline) {
					forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
				} else {
					forceUpdateResp.Failure = append(forceUpdateResp.Failure, sid)
				}
			} else {
				forceUpdateResp.Success = append(forceUpdateResp.Success, sid)
			}
		} else {
			forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
		}
	}

	return forceUpdateResp, nil
}

// Get server config
// @Summary Get server config
// @Security BearerAuth
// @Schemes
// @Description Get server config
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[string]
// @Router /server/config/{id} [get]
func getServerConfig(c *gin.Context) (string, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return "", err
	}

	s, ok := singleton.ServerShared.Get(id)
	// Foreign and unknown IDs deliberately share one response so sequential
	// server IDs cannot be enumerated across tenants.
	if !ok || !s.HasPermission(c) || !patAllowsServer(c, id) {
		return "", singleton.Localizer.ErrorT("permission denied")
	}
	releaseControl, controlErr := beginServerControl(c, id)
	if controlErr != nil {
		return "", controlErr
	}
	releaseControl() // Waiting for ConfigCache must not block ownership rollback.
	if s.GetTaskStream() == nil {
		return "", nil
	}

	if err := s.SendTaskGuarded(&pb.Task{
		Type: model.TaskTypeReportConfig,
	}, serverTaskAdmission(c, id)); err != nil {
		if errors.Is(err, model.ErrTaskStreamOffline) {
			return "", nil
		}
		return "", err
	}

	timeout := time.NewTimer(time.Second * 10)
	defer timeout.Stop()
	select {
	case <-c.Request.Context().Done():
		return "", c.Request.Context().Err()
	case <-timeout.C:
		return "", singleton.Localizer.ErrorT("operation timeout")
	case data := <-s.ConfigCache:
		switch data := data.(type) {
		case string:
			return data, nil
		case error:
			return "", singleton.Localizer.ErrorT("get server config failed: %v", data)
		}
	}

	return "", singleton.Localizer.ErrorT("get server config failed")
}

// Set server config
// @Summary Set server config
// @Security BearerAuth
// @Schemes
// @Description Set server config
// @Tags auth required
// @Accept json
// @Param body body model.ServerConfigForm true "ServerConfigForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServerTaskResponse]
// @Router /server/config [post]
func setServerConfig(c *gin.Context) (*model.ServerTaskResponse, error) {
	var configForm model.ServerConfigForm
	if err := c.ShouldBindJSON(&configForm); err != nil {
		return nil, err
	}

	var resp model.ServerTaskResponse
	slist := singleton.ServerShared.GetList()
	servers := make([]*model.Server, 0, len(configForm.Servers))
	for _, sid := range configForm.Servers {
		s, ok := slist[sid]
		// Match getServerConfig: do not reveal whether a denied ID exists.
		if !ok || !s.HasPermission(c) || !patAllowsServer(c, sid) {
			return nil, singleton.Localizer.ErrorT("permission denied")
		}
		if s.GetTaskStream() == nil {
			resp.Offline = append(resp.Offline, s.ID)
			continue
		}
		servers = append(servers, s)
	}

	var wg sync.WaitGroup
	var respMu sync.Mutex

	for i := 0; i < len(servers); i += 10 {
		end := min(i+10, len(servers))
		group := servers[i:end]

		wg.Add(1)
		go func(srvGroup []*model.Server) {
			defer wg.Done()
			for _, s := range srvGroup {
				task := &pb.Task{
					Type: model.TaskTypeApplyConfig,
					Data: configForm.Config,
				}
				if s.GetTaskStream() == nil {
					respMu.Lock()
					resp.Offline = append(resp.Offline, s.ID)
					respMu.Unlock()
					continue
				}
				// The transport can still be sending after its deadline returns.
				// Keep ownership admission with the actual Send, not the HTTP call.
				sendErr := s.SendTaskGuarded(task, serverTaskAdmission(c, s.ID))
				if err := sendErr; err != nil {
					respMu.Lock()
					if errors.Is(err, model.ErrTaskStreamOffline) {
						resp.Offline = append(resp.Offline, s.ID)
					} else {
						resp.Failure = append(resp.Failure, s.ID)
					}
					respMu.Unlock()
					continue
				}
				respMu.Lock()
				resp.Success = append(resp.Success, s.ID)
				respMu.Unlock()
			}
		}(group)
	}

	wg.Wait()
	return &resp, nil
}

// Batch move servers to other user
// @Summary Batch move servers to other user
// @Security BearerAuth
// @Schemes
// @Description Initiates one ServerTransfer per requested server and returns a
// @Description per-server result. The old behaviour flipped Server.UserID in
// @Description a single SQL UPDATE without telling the agent, so the agent
// @Description kept presenting its old AgentSecret — which now belonged to a
// @Description different user — and authorizeAgentForUUID dropped it. The
// @Description current flow writes a Pending ServerTransfer row and flips
// @Description Server.UserID to the target owner immediately; that row keeps
// @Description the old owner's AgentSecret acceptable for this UUID until the
// @Description agent reconnects under the new secret (MarkVerified clears the
// @Description pending window) or the transfer Cancel/Fail/Timeout-out and
// @Description reverts Server.UserID to the source owner.
// @Tags auth required
// @Accept json
// @Param request body model.BatchMoveServerForm true "BatchMoveServerForm"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.BatchMoveServerResult]
// @Router /batch-move/server [post]
func batchMoveServer(c *gin.Context) ([]model.BatchMoveServerResult, error) {
	var moveForm model.BatchMoveServerForm
	if err := c.ShouldBindJSON(&moveForm); err != nil {
		return nil, err
	}

	if moveForm.ToUser == 0 {
		return nil, singleton.Localizer.ErrorT("user id is required")
	}

	if !callerIsAdmin(c) && moveForm.ToUser != getUid(c) {
		return nil, singleton.Localizer.ErrorT("permission denied")
	}

	singleton.UserLock.RLock()
	_, toUserExists := singleton.UserInfoMap[moveForm.ToUser]
	singleton.UserLock.RUnlock()
	if !toUserExists {
		return nil, singleton.Localizer.ErrorT("user id %d does not exist", moveForm.ToUser)
	}

	results := make([]model.BatchMoveServerResult, 0, len(moveForm.Ids))
	uid := getUid(c)
	isAdmin := callerIsAdmin(c)

	for _, sid := range moveForm.Ids {
		res := model.BatchMoveServerResult{ServerID: sid}

		srv, ok := singleton.ServerShared.Get(sid)
		if !ok || srv == nil {
			res.Status = model.BatchMoveServerResultServerNotFound
			results = append(results, res)
			continue
		}

		// PAT server_ids 白名单优先于 admin/owner 早返回：admin 给自己签发的
		// 限定 server_ids PAT 必须只能 move 白名单内 server。前面的 admin/owner
		// 检查只看 currentOwner，不会触达白名单，这里显式补一道。返回
		// ServerNotFound 与未知/外部 server 的语义对齐，避免泄露白名单外
		// server 是否存在。
		if !patAllowsServer(c, sid) {
			res.Status = model.BatchMoveServerResultServerNotFound
			results = append(results, res)
			continue
		}

		// Per-server permission: admin or current owner. We do NOT use the
		// bulk CheckPermission because we want a partial-success response
		// rather than rejecting the whole batch on the first unauthorized id.
		//
		// 必须走 GetUserID() 而不是裸读 srv.UserID — ServerTransfer.Register
		// 和 revertTransition 会通过 atomic.StoreUint64 改写当前 Server.UserID
		// 以反映新所有者。batchMoveServer 与 transfer 流程并发时（典型场景：两
		// 个 operator 几乎同时发起 move），裸读会与 SetUserID 形成 data race，
		// 且可能在 transfer 切换瞬间读到过期值并据此做权限/同所有者/fromUser
		// 判断。
		currentOwner := srv.GetUserID()
		if !isAdmin && currentOwner != uid {
			// Match the unknown-id response for members. A distinct
			// permission_denied result lets callers enumerate foreign server IDs.
			res.Status = model.BatchMoveServerResultServerNotFound
			results = append(results, res)
			continue
		}

		if currentOwner == moveForm.ToUser {
			res.Status = model.BatchMoveServerResultSameOwner
			results = append(results, res)
			continue
		}

		// One active ServerTransfer per server. InitiateExclusive serializes
		// the HasPending guard, the DB transaction, and the in-memory
		// Register under a per-server claim so two concurrent operators
		// can't both observe "no pending", both insert, and silently end up
		// with two Pending rows for the same server.
		fromUser := currentOwner
		created, err := singleton.ServerTransferShared.InitiateExclusive(sid, fromUser, moveForm.ToUser, uid)
		if err != nil {
			switch {
			case errors.Is(err, singleton.ErrServerAlreadyTransferring):
				res.Status = model.BatchMoveServerResultAlreadyTransferring
			case errors.Is(err, singleton.ErrAgentTooOldForTransfer):
				res.Status = model.BatchMoveServerResultAgentTooOld
				res.Error = err.Error()
			default:
				res.Status = model.BatchMoveServerResultServerNotFound
				res.Error = err.Error()
			}
			results = append(results, res)
			continue
		}

		singleton.ServerTransferShared.PushIfOnline(created)

		res.Status = model.BatchMoveServerResultPending
		res.TransferID = created.ID
		results = append(results, res)
	}

	return results, nil
}

var serverMetricMap = map[string]tsdb.MetricType{
	"cpu":              tsdb.MetricServerCPU,
	"memory":           tsdb.MetricServerMemory,
	"swap":             tsdb.MetricServerSwap,
	"disk":             tsdb.MetricServerDisk,
	"net_in_speed":     tsdb.MetricServerNetInSpeed,
	"net_out_speed":    tsdb.MetricServerNetOutSpeed,
	"net_in_transfer":  tsdb.MetricServerNetInTransfer,
	"net_out_transfer": tsdb.MetricServerNetOutTransfer,
	"load1":            tsdb.MetricServerLoad1,
	"load5":            tsdb.MetricServerLoad5,
	"load15":           tsdb.MetricServerLoad15,
	"tcp_conn":         tsdb.MetricServerTCPConn,
	"udp_conn":         tsdb.MetricServerUDPConn,
	"process_count":    tsdb.MetricServerProcessCount,
	"temperature":      tsdb.MetricServerTemperature,
	"uptime":           tsdb.MetricServerUptime,
	"gpu":              tsdb.MetricServerGPU,
}

// Get server metrics history
// @Summary Get server metrics history
// @Security BearerAuth
// @Schemes
// @Description Get server metrics history for a specific server
// @Tags common
// @param id path uint true "Server ID"
// @param metric query string true "Metric name: cpu, memory, swap, disk, net_in_speed, net_out_speed, net_in_transfer, net_out_transfer, load1, load5, load15, tcp_conn, udp_conn, process_count, temperature, uptime, gpu"
// @param period query string false "Time period: 1d, 7d, 30d (default: 1d)"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.ServerMetricsResponse]
// @Router /server/{id}/metrics [get]
func getServerMetrics(c *gin.Context) (*model.ServerMetricsResponse, error) {
	idStr := c.Param("id")
	serverID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return nil, err
	}

	server, ok := singleton.ServerShared.Get(serverID)
	if !ok {
		return nil, singleton.Localizer.ErrorT("server not found")
	}

	if !userCanViewServer(c, server) {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	_, isMember := c.Get(model.CtxKeyAuthorizedUser)

	metricName := c.Query("metric")
	metricType, ok := serverMetricMap[metricName]
	if !ok {
		return nil, singleton.Localizer.ErrorT("invalid metric name")
	}

	periodStr := c.DefaultQuery("period", "1d")
	period, err := tsdb.ParseQueryPeriod(periodStr)
	if err != nil {
		return nil, err
	}

	if !isMember && period != tsdb.Period1Day {
		return nil, singleton.Localizer.ErrorT("unauthorized: only 1d data available for guests")
	}

	response := &model.ServerMetricsResponse{
		ServerID:   serverID,
		ServerName: server.Name,
		Metric:     metricName,
		DataPoints: make([]model.ServerMetricsDataPoint, 0),
	}

	if !singleton.TSDBEnabled() {
		return response, nil
	}

	points, err := singleton.TSDBShared.QueryServerMetrics(serverID, metricType, period)
	if err != nil {
		return nil, err
	}

	response.DataPoints = points

	return response, nil
}
