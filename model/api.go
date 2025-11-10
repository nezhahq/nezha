package model

const (
	ApiErrorUnauthorized = 10001
)

type Oauth2LoginResponse struct {
	Redirect string `json:"redirect,omitempty"`
}

type Oauth2Callback struct {
	State string `json:"state,omitempty"`
	Code  string `json:"code,omitempty"`
}

type LoginRequest struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type CommonResponse[T any] struct {
	Success bool   `json:"success,omitempty"`
	Data    T      `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

type PaginatedResponse[S ~[]E, E any] struct {
	Success bool      `json:"success,omitempty"`
	Data    *Value[S] `json:"data,omitempty"`
	Error   string    `json:"error,omitempty"`
}

type Value[T any] struct {
	Value      T          `json:"value,omitempty"`
	Pagination Pagination `json:"pagination,omitempty"`
}

type Pagination struct {
	Offset int   `json:"offset,omitempty"`
	Limit  int   `json:"limit,omitempty"`
	Total  int64 `json:"total,omitempty"`
}

type LoginResponse struct {
	Token  string `json:"token,omitempty"`
	Expire string `json:"expire,omitempty"`
}
