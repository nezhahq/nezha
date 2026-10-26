package rpc

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"github.com/hashicorp/go-uuid"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	serviceRPC "github.com/nezhahq/nezha/service/rpc"
	"github.com/nezhahq/nezha/service/singleton"
)

var natActiveRequests = make(chan struct{}, 128)

func ServeNAT(w http.ResponseWriter, r *http.Request, natConfig *model.NAT) {
	// Reject unsupported protocols before allocating streams or tasking the agent.
	if _, ok := w.(http.Hijacker); !ok {
		http.Error(w, "NAT requires HTTP/1.1", http.StatusHTTPVersionNotSupported)
		return
	}
	if r.ContentLength > utils.NATMaxBodyBytes {
		http.Error(w, "NAT body too large", http.StatusRequestEntityTooLarge)
		return
	}
	singleton.UserLock.RLock()
	owner, known := singleton.UserInfoMap[natConfig.UserID]
	singleton.UserLock.RUnlock()
	admin := known && owner.Role.IsAdmin()
	release, _, accessErr := singleton.BeginServerControl(natConfig.ServerID, natConfig.UserID, admin)
	if !known || accessErr != nil || !singleton.NATDomainAllowed(natConfig.Domain, admin) {
		if release != nil {
			release()
		}
		http.Error(w, "NAT permission denied", http.StatusForbidden)
		return
	}
	var releaseOnce sync.Once
	releaseControl := func() { releaseOnce.Do(release) }
	defer releaseControl()
	select {
	case natActiveRequests <- struct{}{}:
		defer func() { <-natActiveRequests }()
	default:
		http.Error(w, "NAT capacity exceeded", http.StatusTooManyRequests)
		return
	}

	capabilityLease, capabilityErr := prepareNATCapability(r, natConfig)
	if capabilityErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("NAT capability unavailable"))
		return
	}
	handler := serviceRPC.NezhaHandlerSingleton
	streamId := ""
	legacyStreamOwned := false
	relayCompleted := false
	defer func() {
		if capabilityLease.active {
			if !relayCompleted {
				capabilityLease.cleanup(handler)
			}
			return
		}
		if legacyStreamOwned {
			_ = handler.CloseStream(streamId)
		}
	}()
	server, _ := singleton.ServerShared.Get(natConfig.ServerID)
	if server == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("server not found or not connected"))
		return
	}
	if server.GetTaskStream() == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("server not found or not connected"))
		return
	}

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "stream id error: %v", err))
		return
	}
	if capabilityLease.active {
		capabilityLease.streamLease, err = handler.CreateAgentCompatNATStream(capabilityLease.handle, streamId)
	} else {
		err = handler.CreateStream(streamId, 0, server.ID)
	}
	if err != nil {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(fmt.Appendf(nil, "stream limit: %v", err))
		return
	}
	legacyStreamOwned = !capabilityLease.active
	taskData, err := json.Marshal(model.TaskNAT{StreamID: streamId, Host: natConfig.Host})
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "task data error: %v", err))
		return
	}

	if err := server.SendTask(&proto.Task{Type: model.TaskTypeNAT, Data: string(taskData)}); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(fmt.Appendf(nil, "send task error: %v", err))
		return
	}

	releaseControl() // The registered stream is now covered by transfer revocation.

	// Credential invariant at NAT ingress: credentials issued by this
	// dashboard — the panel-signed JWT (verified by signature only, expiry
	// deliberately ignored) and the panel API token — must never become origin
	// credentials for the configured NAT backend. prepareNATCapability has
	// already stripped them from every channel the panel's own TokenLookup
	// accepts (Authorization header, ?token= query parameter, nz-jwt cookie);
	// whatever remains on those channels is either absent or a foreign
	// credential, which is forwarded untouched because the NAT backend may
	// authenticate with credentials of its own.
	wWrapped, err := utils.NewRequestWrapper(r, w)
	if err != nil {
		return
	}

	requestContext, cancel := context.WithTimeout(r.Context(), utils.NATMaxDuration)
	defer cancel()
	stop := context.AfterFunc(requestContext, func() { _ = wWrapped.Close() })
	defer stop()
	if err := handler.UserConnected(streamId, wWrapped); err != nil {
		if closeErr := wWrapped.Close(); closeErr != nil {
			log.Printf("NEZHA>> NAT request wrapper close error after user connection failure: %v", closeErr)
		}
		return
	}

	if capabilityLease.active {
		if err := handler.PublishAgentCompatNATStream(capabilityLease.handle, serviceRPC.AgentCompatNATPublication{
			Purpose:        serviceRPC.AgentCompatCapabilityNAT,
			TargetServerID: server.ID,
			ResourceID:     natConfig.ID,
			StreamID:       streamId,
		}); err != nil {
			return
		}
	}
	if capabilityLease.active {
		capabilityLease.publicationOwned, err = handler.StartAgentCompatNATStream(capabilityLease.handle, time.Second*10)
		if err != nil {
			return
		}
	} else if err := handler.StartStream(streamId, time.Second*10); err != nil {
		return
	}
	relayCompleted = true
}
