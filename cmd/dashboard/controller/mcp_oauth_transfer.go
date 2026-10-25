package controller

import (
	"errors"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func revalidateMCPOAuthTransfer(entry *transferEntry) error {
	issuer, err := mcpOAuthIssuer()
	if err != nil {
		return err
	}
	var grant model.MCPOAuthGrant
	if singleton.DB.First(&grant, "id = ?", entry.OAuthGrantID).Error != nil || grant.APITokenID != entry.TokenID || grant.Resource != issuer+"/mcp" || !time.Now().Before(grant.AccessExpiresAt) || !mcpOAuthHashEqual(grant.AccessHash, entry.OAuthAccessHash) {
		return errors.New("originating OAuth credentials no longer valid")
	}
	permission, _, err := mcpOAuthGrantIdentity(singleton.DB, &grant)
	if err != nil || permission.UserID != entry.UserID {
		return errors.New("originating OAuth authorization no longer valid")
	}
	return nil
}
