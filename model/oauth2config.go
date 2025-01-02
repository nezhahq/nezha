package model

import (
	"fmt"
	"golang.org/x/oauth2"
	"regexp"
	"strings"
)

type Oauth2Config struct {
	ClientID     string         `mapstructure:"client_id" json:"client_id,omitempty"`
	ClientSecret string         `mapstructure:"client_secret" json:"client_secret,omitempty"`
	Endpoint     Oauth2Endpoint `mapstructure:"endpoint" json:"endpoint,omitempty"`
	Scopes       []string       `mapstructure:"scopes" json:"scopes,omitempty"`

	UserInfoURL string `mapstructure:"user_info_url" json:"user_info_url,omitempty"`
	UserIDPath  string `mapstructure:"user_id_path" json:"user_id_path,omitempty"`
}

type Oauth2Endpoint struct {
	AuthURL  string `mapstructure:"auth_url" json:"auth_url,omitempty"`
	TokenURL string `mapstructure:"token_url" json:"token_url,omitempty"`
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

// AddFmtParamIfNeeded 添加 fmt 参数到 URL 如果授权方式为 QQ
func (c *Oauth2Config) AddFmtParamIfNeeded(authMethod string) string {
	// 正则表达式匹配 QQ 相关的授权方式
	re := regexp.MustCompile(`(?i)qq`)
	if re.MatchString(authMethod) {
		// 如果匹配，则添加 fmt=json 参数
		return fmt.Sprintf("%s&fmt=json", c.UserInfoURL)
	}
	// 如果不匹配，则返回原始 URL
	return c.UserInfoURL
}

// GetUserInfo 使用 access_token 获取用户信息，并根据需要添加 fmt 参数
func GetUserInfo(access_token, openid, authMethod string) (string, error) {
	// 获取配置实例，这里假设您已经有了一个全局的配置实例
	config := &Oauth2Config{
		// 填充您的配置信息
	}
	urlWithFmt := config.AddFmtParamIfNeeded(authMethod)

	// 发起请求获取用户信息
	response, err := http.Get(urlWithFmt)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	// 检查响应状态码
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get user info, status code: %d", response.StatusCode)
	}

	// 读取响应内容
	contents, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}

	return string(contents), nil
}
