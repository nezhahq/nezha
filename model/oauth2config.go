package model

import (
	"golang.org/x/oauth2"
)

type Oauth2Config struct {
	ClientID     string         `koanf:"client_id" json:"client_id,omitempty"`
	ClientSecret string         `koanf:"client_secret" json:"client_secret,omitempty"`
	Endpoint     Oauth2Endpoint `koanf:"endpoint" json:"endpoint,omitempty"`
	Scopes       []string       `koanf:"scopes" json:"scopes,omitempty"`

	UserInfoURL string `koanf:"user_info_url" json:"user_info_url,omitempty"`
	UserIDPath  string `koanf:"user_id_path" json:"user_id_path,omitempty"`
}

type Oauth2Endpoint struct {
	AuthURL  string `koanf:"auth_url" json:"auth_url,omitempty"`
	TokenURL string `koanf:"token_url" json:"token_url,omitempty"`
}

func (c *Oauth2Config) Setup(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  c.Endpoint.AuthURL,
			TokenURL: c.Endpoint.TokenURL,
		},
		RedirectURL: redirectURL,
		Scopes:      c.Scopes,
	}
}
