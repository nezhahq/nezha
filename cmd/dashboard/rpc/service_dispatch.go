package rpc

import (
	"errors"
	"log"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

func DispatchTask(serviceSentinelDispatchBus <-chan *model.Service) {
	for task := range serviceSentinelDispatchBus {
		if task == nil {
			continue
		}
		if err := model.ValidateServiceMonitorType(uint64(task.Type)); err != nil {
			// Defense in depth for stale database rows and future internal callers:
			// Service.Type shares its integer namespace with command/config tasks.
			log.Printf("NEZHA>> DispatchTask rejected service %d: %v", task.ID, err)
			continue
		}
		probe := task.PB()
		if probe == nil {
			log.Printf("NEZHA>> DispatchTask rejected service %d: invalid probe", task.ID)
			continue
		}

		switch task.Cover {
		case model.ServiceCoverIgnoreAll:
			for id, enabled := range task.SkipServers {
				if !enabled {
					continue
				}

				server, _ := singleton.ServerShared.Get(id)
				if server == nil {
					continue
				}
				if !canSendTaskToServer(task, server) {
					continue
				}
				if err := server.QueueTaskGuarded(probe, singleton.ServerTaskAdmission(server.ID, task.UserID)); err != nil && !errors.Is(err, model.ErrTaskStreamOffline) {
					log.Printf("NEZHA>> DispatchTask send error (server=%d): %v", id, err)
				}
			}
		case model.ServiceCoverAll:
			for id, server := range singleton.ServerShared.GetList() {
				if server == nil || task.SkipServers[id] {
					continue
				}
				if !canSendTaskToServer(task, server) {
					continue
				}
				if err := server.QueueTaskGuarded(probe, singleton.ServerTaskAdmission(server.ID, task.UserID)); err != nil && !errors.Is(err, model.ErrTaskStreamOffline) {
					log.Printf("NEZHA>> DispatchTask send error (server=%d): %v", id, err)
				}
			}
		}
	}
}

func DispatchKeepalive() {
	singleton.CronShared.AddFunc("@every 20s", func() {
		list := singleton.ServerShared.GetSortedList()
		for _, s := range list {
			if s == nil {
				continue
			}
			if err := s.QueueTask(&proto.Task{Type: model.TaskTypeKeepalive}); err != nil && !errors.Is(err, model.ErrTaskStreamOffline) {
				log.Printf("NEZHA>> Keepalive send error (server=%d): %v", s.ID, err)
			}
		}
	})
}

func canSendTaskToServer(task *model.Service, server *model.Server) bool {
	var role model.Role
	singleton.UserLock.RLock()
	if u, ok := singleton.UserInfoMap[task.UserID]; !ok {
		role = model.RoleMember
	} else {
		role = u.Role
	}
	singleton.UserLock.RUnlock()

	return task.UserID == server.GetUserID() || role.IsAdmin()
}
