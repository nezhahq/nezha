package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func mcpOAuthManagementHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	c.Next()
}

func mcpOAuthRequireEnabledClient(tx *gorm.DB, id string) error {
	var count int64
	if err := tx.Model(&model.MCPOAuthClient{}).Where("id = ? AND disabled = ?", id, false).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errMCPOAuthInvalidGrant
	}
	return nil
}

func mcpOAuthManagementPagination(c *gin.Context) (int, int, error) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 || page > 1000000 {
		return 0, 0, errors.New("invalid page")
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		return 0, 0, errors.New("invalid limit")
	}
	return (page - 1) * limit, limit, nil
}

type mcpOAuthGrantView struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	UserID     uint64     `json:"user_id"`
	Username   string     `json:"username"`
	Scopes     []string   `json:"scopes"`
	ServerIDs  []uint64   `json:"server_ids"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	Status     string     `json:"status"`
}

func listMCPOAuthGrants(c *gin.Context) (any, error) {
	all, err := strconv.ParseBool(c.DefaultQuery("all", "false"))
	if err != nil {
		return nil, errors.New("invalid all parameter")
	}
	if all && !callerIsAdmin(c) {
		return nil, errors.New("permission denied")
	}
	offset, limit, err := mcpOAuthManagementPagination(c)
	if err != nil {
		return nil, err
	}
	// Select a dedicated view, never serialize credential models or their hashes.
	query := singleton.DB.Table("mcp_oauth_grants AS g").
		Joins("JOIN api_tokens AS p ON p.id = g.api_token_id").
		Joins("JOIN users AS u ON u.id = p.user_id").
		Joins("LEFT JOIN mcp_oauth_clients AS c ON c.id = g.client_id")
	if !all {
		query = query.Where("p.user_id = ?", getUid(c))
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	var rows []struct {
		ID, ClientID, ClientName, Username, ScopesCSV, ServersCSV, LastUsedIP string
		Resource, RegisteredClientID                                          string
		UserID, UserVersion, CurrentUserVersion                               uint64
		Disabled                                                              bool
		CreatedAt, ExpiresAt                                                  time.Time
		LastUsedAt, PermissionExpiresAt                                       *time.Time
	}
	err = query.Select("g.id, g.client_id, c.name AS client_name, p.user_id, u.username, p.scopes_csv, p.servers_csv, p.created_at, g.expires_at, p.last_used_at, p.last_used_ip, g.resource, g.user_version, u.token_version AS current_user_version, c.disabled, c.id AS registered_client_id, p.expires_at AS permission_expires_at").
		Order("p.id DESC").Offset(offset).Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, newGormError("%v", err)
	}
	items := make([]mcpOAuthGrantView, 0, len(rows))
	issuer, issuerErr := mcpOAuthIssuer()
	now := time.Now()
	for _, row := range rows {
		permission := model.APIToken{ScopesCSV: row.ScopesCSV, ServersCSV: row.ServersCSV, ExpiresAt: row.PermissionExpiresAt}
		status := "active"
		if !now.Before(row.ExpiresAt) || permission.IsExpired(now) {
			status = "expired"
		} else if row.Disabled || row.RegisteredClientID == "" || row.UserVersion != row.CurrentUserVersion || issuerErr != nil || row.Resource != issuer+"/mcp" {
			status = "unavailable"
		}
		scopes, servers := permission.Scopes(), permission.ServerIDs()
		if scopes == nil {
			scopes = []string{}
		}
		if servers == nil {
			servers = []uint64{}
		}
		items = append(items, mcpOAuthGrantView{ID: row.ID, ClientID: row.ClientID, ClientName: row.ClientName, UserID: row.UserID, Username: row.Username, Scopes: scopes, ServerIDs: servers, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt, LastUsedIP: row.LastUsedIP, Status: status})
	}
	return gin.H{"items": items, "total": total}, nil
}

func revokeMCPOAuthGrant(c *gin.Context) (any, error) {
	var tokenID uint64
	err := mcpOAuthTransaction(func(tx *gorm.DB) error {
		query := tx.Table("mcp_oauth_grants AS g").Select("g.*").Joins("JOIN api_tokens AS p ON p.id = g.api_token_id").Where("g.id = ?", c.Param("id"))
		if !callerIsAdmin(c) {
			query = query.Where("p.user_id = ?", getUid(c))
		}
		var grant model.MCPOAuthGrant
		if err := query.Take(&grant).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("authorization not found")
			}
			return err
		}
		tokenID = grant.APITokenID
		return mcpOAuthDeleteGrant(tx, &grant)
	})
	if err != nil {
		return nil, err
	}
	patConnectionRegistryShared.revokeToken(tokenID)
	return nil, nil
}

type mcpOAuthClientView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	AuthMethod   string    `json:"auth_method"`
	Disabled     bool      `json:"disabled"`
	LastUsedAt   time.Time `json:"last_used_at"`
	GrantCount   int64     `json:"grant_count"`
}

func listMCPOAuthClients(c *gin.Context) (any, error) {
	offset, limit, err := mcpOAuthManagementPagination(c)
	if err != nil {
		return nil, err
	}
	var total int64
	if err := singleton.DB.Model(&model.MCPOAuthClient{}).Count(&total).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	var rows []struct {
		ID, Name, RedirectURIs, AuthMethod string
		Disabled                           bool
		LastUsedAt                         time.Time
		GrantCount                         int64
	}
	err = singleton.DB.Table("mcp_oauth_clients AS c").Select("c.id, c.name, c.redirect_uris, c.auth_method, c.disabled, c.last_used_at, (SELECT COUNT(*) FROM mcp_oauth_grants g JOIN api_tokens p ON p.id = g.api_token_id WHERE g.client_id = c.id) AS grant_count").Order("c.last_used_at DESC, c.id ASC").Offset(offset).Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, newGormError("%v", err)
	}
	items := make([]mcpOAuthClientView, 0, len(rows))
	for _, row := range rows {
		redirects := []string{}
		if err := json.Unmarshal([]byte(row.RedirectURIs), &redirects); err != nil {
			return nil, errors.New("invalid stored client redirects")
		}
		items = append(items, mcpOAuthClientView{ID: row.ID, Name: row.Name, RedirectURIs: redirects, AuthMethod: row.AuthMethod, Disabled: row.Disabled, LastUsedAt: row.LastUsedAt, GrantCount: row.GrantCount})
	}
	return gin.H{"items": items, "total": total}, nil
}

func updateMCPOAuthClient(c *gin.Context) (any, error) {
	var request struct {
		Disabled *bool `json:"disabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		return nil, err
	}
	var revoked []uint64
	err := mcpOAuthTransaction(func(tx *gorm.DB) error {
		updated := tx.Model(&model.MCPOAuthClient{}).Where("id = ?", c.Param("id")).Update("disabled", *request.Disabled)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("client not found")
		}
		if !*request.Disabled {
			return nil
		}
		// Disable and revoke atomically. Re-enabling must never revive credentials
		// or unexchanged codes that existed before the administrator's decision.
		grants := tx.Model(&model.MCPOAuthGrant{}).Where("client_id = ?", c.Param("id"))
		if err := grants.Pluck("api_token_id", &revoked).Error; err != nil {
			return err
		}
		if err := tx.Where("id IN (?)", tx.Model(&model.MCPOAuthGrant{}).Select("api_token_id").Where("client_id = ?", c.Param("id"))).Delete(&model.APIToken{}).Error; err != nil {
			return err
		}
		if err := tx.Where("client_id = ?", c.Param("id")).Delete(&model.MCPOAuthGrant{}).Error; err != nil {
			return err
		}
		return tx.Where("client_id = ?", c.Param("id")).Delete(&model.MCPOAuthCode{}).Error
	})
	if err != nil {
		return nil, err
	}
	for _, id := range revoked {
		patConnectionRegistryShared.revokeToken(id)
	}
	return nil, nil
}
