package controller

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

// List server
// @Summary List server
// @Security BearerAuth
// @Schemes
// @Description List server
// @Tags auth required
// @Param id query uint false "Resource ID"
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.Server]
// @Router /server [get]
func listServer(c *gin.Context) ([]*model.Server, error) {
	singleton.SortedServerLock.RLock()
	defer singleton.SortedServerLock.RUnlock()

	var ssl []*model.Server
	if err := copier.Copy(&ssl, &singleton.SortedServerList); err != nil {
		return nil, err
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

	singleton.DDNSCacheLock.RLock()
	for _, pid := range sf.DDNSProfiles {
		if p, ok := singleton.DDNSCache[pid]; ok {
			if !p.HasPermission(c) {
				singleton.DDNSCacheLock.RUnlock()
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
		}
	}
	singleton.DDNSCacheLock.RUnlock()

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
	ddnsProfilesRaw, err := utils.Json.Marshal(s.DDNSProfiles)
	if err != nil {
		return nil, err
	}
	s.DDNSProfilesRaw = string(ddnsProfilesRaw)

	if err := singleton.DB.Save(&s).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.ServerLock.Lock()
	s.CopyFromRunningServer(singleton.ServerList[s.ID])
	singleton.ServerList[s.ID] = &s
	singleton.ServerLock.Unlock()
	singleton.ReSortServer()

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

	singleton.ServerLock.RLock()
	for _, sid := range servers {
		if s, ok := singleton.ServerList[sid]; ok {
			if !s.HasPermission(c) {
				singleton.ServerLock.RUnlock()
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
		}
	}
	singleton.ServerLock.RUnlock()

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

	singleton.OnServerDelete(servers)
	singleton.ReSortServer()

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
// @Success 200 {object} model.CommonResponse[model.ForceUpdateResponse]
// @Router /force-update/server [post]
func forceUpdateServer(c *gin.Context) (*model.ForceUpdateResponse, error) {
	var forceUpdateServers []uint64
	if err := c.ShouldBindJSON(&forceUpdateServers); err != nil {
		return nil, err
	}

	forceUpdateResp := new(model.ForceUpdateResponse)

	for _, sid := range forceUpdateServers {
		singleton.ServerLock.RLock()
		server := singleton.ServerList[sid]
		singleton.ServerLock.RUnlock()
		if server != nil && server.TaskStream != nil {
			if !server.HasPermission(c) {
				return nil, singleton.Localizer.ErrorT("permission denied")
			}
			if err := server.TaskStream.Send(&pb.Task{
				Type: model.TaskTypeUpgrade,
			}); err != nil {
				forceUpdateResp.Failure = append(forceUpdateResp.Failure, sid)
			} else {
				forceUpdateResp.Success = append(forceUpdateResp.Success, sid)
			}
		} else {
			forceUpdateResp.Offline = append(forceUpdateResp.Offline, sid)
		}
	}

	return forceUpdateResp, nil
}
