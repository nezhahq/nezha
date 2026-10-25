package model

import "time"

// MCPOAuthClient stores DCR metadata only; client secrets are never recoverable.
type MCPOAuthClient struct {
	ID           string `gorm:"primaryKey;size:64"`
	Name         string
	RedirectURIs string
	AuthMethod   string
	SecretHash   string
	LastUsedAt   time.Time `gorm:"index"`
	Disabled     bool      `gorm:"not null;default:false"`
}

// MCPOAuthCode binds a one-use authorization code to its client, browser
// approval, redirect URI, PKCE challenge, resource and user credential version.
type MCPOAuthCode struct {
	Hash        string `gorm:"primaryKey;size:64"`
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    string
	UserID      uint64
	UserVersion uint64
	ScopesCSV   string
	ServersCSV  string
	ExpiresAt   time.Time `gorm:"index"`
	GrantID     string
}

// MCPOAuthGrant uses an APIToken as the existing permission/audit/revocation
// identity, but its opaque access credentials are accepted only by /mcp.
type MCPOAuthGrant struct {
	ID              string `gorm:"primaryKey;size:64"`
	APITokenID      uint64 `gorm:"uniqueIndex"`
	ClientID        string `gorm:"index"`
	Resource        string
	UserVersion     uint64
	AccessHash      string `gorm:"uniqueIndex;size:64"`
	AccessExpiresAt time.Time
	RefreshHash     string
	ExpiresAt       time.Time `gorm:"index"`
}

func (MCPOAuthClient) TableName() string { return "mcp_oauth_clients" }
func (MCPOAuthCode) TableName() string   { return "mcp_oauth_codes" }
func (MCPOAuthGrant) TableName() string  { return "mcp_oauth_grants" }
