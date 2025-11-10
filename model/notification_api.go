package model

type NotificationForm struct {
	Name          string `json:"name,omitempty" minLength:"1"`
	URL           string `json:"url,omitempty"`
	RequestMethod uint8  `json:"request_method,omitempty"`
	RequestType   uint8  `json:"request_type,omitempty"`
	RequestHeader string `json:"request_header,omitempty"`
	RequestBody   string `json:"request_body,omitempty"`
	VerifyTLS     bool   `json:"verify_tls,omitempty" validate:"optional"`
	SkipCheck     bool   `json:"skip_check,omitempty" validate:"optional"`
}
