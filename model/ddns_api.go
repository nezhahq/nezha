package model

type DDNSForm struct {
	MaxRetries         uint64   `json:"max_retries,omitempty" default:"3"`
	EnableIPv4         bool     `json:"enable_ipv4,omitempty" validate:"optional"`
	EnableIPv6         bool     `json:"enable_ipv6,omitempty" validate:"optional"`
	Name               string   `json:"name,omitempty" minLength:"1"`
	Provider           string   `json:"provider,omitempty"`
	Domains            []string `json:"domains,omitempty"`
	AccessID           string   `json:"access_id,omitempty" validate:"optional"`
	AccessSecret       string   `json:"access_secret,omitempty" validate:"optional"`
	WebhookURL         string   `json:"webhook_url,omitempty" validate:"optional"`
	WebhookMethod      uint8    `json:"webhook_method,omitempty" validate:"optional" default:"1"`
	WebhookRequestType uint8    `json:"webhook_request_type,omitempty" validate:"optional" default:"1"`
	WebhookRequestBody string   `json:"webhook_request_body,omitempty" validate:"optional"`
	WebhookHeaders     string   `json:"webhook_headers,omitempty" validate:"optional"`
}
