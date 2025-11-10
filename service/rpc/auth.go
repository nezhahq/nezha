package rpc

import (
	"context"
	"strings"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/hashicorp/go-uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

type authHandler struct {
	ClientSecret string
	ClientUUID   string
}

func (a *authHandler) Check(ctx context.Context) (uint64, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return 0, status.Errorf(codes.Unauthenticated, "获取 metaData 失败")
	}

	var clientSecret string
	if value, ok := md["client_secret"]; ok {
		clientSecret = strings.TrimSpace(value[0])
	}

	if clientSecret == "" {
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}

	ip, _ := ctx.Value(model.CtxKeyRealIP{}).(string)

	singleton.UserLock.RLock()
	userId, ok := singleton.AgentSecretToUserId[clientSecret]
	if !ok {
		singleton.UserLock.RUnlock()
		model.BlockIP(singleton.DB, ip, model.WAFBlockReasonTypeAgentAuthFail, model.BlockIDgRPC)
		return 0, status.Error(codes.Unauthenticated, "客户端认证失败")
	}
	singleton.UserLock.RUnlock()

	model.UnblockIP(singleton.DB, ip, model.BlockIDgRPC)

	var clientUUID string
	if value, ok := md["client_uuid"]; ok {
		clientUUID = value[0]
	}

	if _, err := uuid.ParseUUID(clientUUID); err != nil {
		return 0, status.Error(codes.Unauthenticated, "客户端 UUID 不合法")
	}

	clientID, hasID := singleton.ServerShared.UUIDToID(clientUUID)
	if !hasID {
		s := model.Server{UUID: clientUUID, Name: petname.Generate(2, "-"), Common: model.Common{
			UserID: userId,
		}}
		if err := singleton.DB.Create(&s).Error; err != nil {
			return 0, status.Error(codes.Unauthenticated, err.Error())
		}

		model.InitServer(&s)
		singleton.ServerShared.Update(&s, clientUUID)

		clientID = s.ID
	}

	return clientID, nil
}
