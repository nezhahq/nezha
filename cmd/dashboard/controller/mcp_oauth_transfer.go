package controller

import (
	"errors"
	"time"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func revalidateMCPOAuthTransfer(entry *transferEntry) error {
	return revalidateMCPOAuthCredential(entry.OAuthGrantID, entry.OAuthAccessHash, entry.TokenID, entry.UserID)
}

// revalidateMCPOAuthCredential re-derives, for work that outlives the request
// head it was authenticated on (a transfer URL, a tools/call whose body arrives
// later), whether the access token presented back then would still be accepted.
func revalidateMCPOAuthCredential(grantID, accessHash string, tokenID, userID uint64) error {
	issuer, err := mcpOAuthIssuer()
	if err != nil {
		return err
	}
	var grant model.MCPOAuthGrant
	if singleton.DB.First(&grant, "id = ?", grantID).Error != nil || grant.APITokenID != tokenID || grant.Resource != issuer+"/mcp" || !time.Now().Before(grant.AccessExpiresAt) || !mcpOAuthHashEqual(grant.AccessHash, accessHash) {
		return errors.New("originating OAuth credentials no longer valid")
	}
	permission, _, err := mcpOAuthGrantIdentity(singleton.DB, &grant)
	if err != nil || permission.UserID != userID {
		return errors.New("originating OAuth authorization no longer valid")
	}
	return nil
}
