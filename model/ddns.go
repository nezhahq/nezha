package model

import (
	"github.com/goccy/go-json"
	"gorm.io/gorm"
)

const (
	ProviderDummy        = "dummy"
	ProviderWebHook      = "webhook"
	ProviderCloudflare   = "cloudflare"
	ProviderTencentCloud = "tencentcloud"
	ProviderHE           = "he"
)

var ProviderList = [...]string{
	ProviderDummy, ProviderWebHook, ProviderCloudflare, ProviderTencentCloud, ProviderHE,
}

type DDNSProfile struct {
	Common
	EnableIPv4         *bool    `json:"enable_ipv4,omitempty"`
	EnableIPv6         *bool    `json:"enable_ipv6,omitempty"`
	MaxRetries         uint64   `json:"max_retries"`
	Name               string   `json:"name"`
	Provider           string   `json:"provider"`
	AccessID           string   `json:"access_id,omitempty"`
	AccessSecret       string   `json:"access_secret,omitempty"`
	WebhookURL         string   `json:"webhook_url,omitempty"`
	WebhookMethod      uint8    `json:"webhook_method,omitempty"`
	WebhookRequestType uint8    `json:"webhook_request_type,omitempty"`
	WebhookRequestBody string   `json:"webhook_request_body,omitempty"`
	WebhookHeaders     string   `json:"webhook_headers,omitempty"`
	Domains            []string `json:"domains" gorm:"-"`
	DomainsRaw         string   `json:"-"`
}

func (d *DDNSProfile) TableName() string {
	return "ddns"
}

func (d *DDNSProfile) BeforeSave(tx *gorm.DB) error {
	if data, err := json.Marshal(d.Domains); err != nil {
		return err
	} else {
		d.DomainsRaw = string(data)
	}
	return nil
}

func (d *DDNSProfile) AfterFind(tx *gorm.DB) error {
	return json.Unmarshal([]byte(d.DomainsRaw), &d.Domains)
}
