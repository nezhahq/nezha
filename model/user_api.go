package model

type UserForm struct {
	Role     Role   `json:"role,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty" gorm:"type:char(72)"`
}

type ProfileForm struct {
	OriginalPassword string `json:"original_password,omitempty"`
	NewUsername      string `json:"new_username,omitempty"`
	NewPassword      string `json:"new_password,omitempty"`
	RejectPassword   bool   `json:"reject_password,omitempty" validate:"optional"`
}
