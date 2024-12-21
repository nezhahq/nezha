package controller

import (
	"slices"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

// Get profile
// @Summary Get profile
// @Security BearerAuth
// @Schemes
// @Description Get profile
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[model.Profile]
// @Router /profile [get]
func getProfile(c *gin.Context) (*model.Profile, error) {
	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}
	return &model.Profile{
		User:    *auth.(*model.User),
		LoginIP: c.GetString(model.CtxKeyRealIPStr),
	}, nil
}

// Update password for current user
// @Summary Update password for current user
// @Security BearerAuth
// @Schemes
// @Description Update password for current user
// @Tags auth required
// @Accept json
// @param request body model.ProfileForm true "password"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /profile [post]
func updateProfile(c *gin.Context) (any, error) {
	var pf model.ProfileForm
	if err := c.ShouldBindJSON(&pf); err != nil {
		return 0, err
	}

	auth, ok := c.Get(model.CtxKeyAuthorizedUser)
	if !ok {
		return nil, singleton.Localizer.ErrorT("unauthorized")
	}

	user := *auth.(*model.User)
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(pf.OriginalPassword)); err != nil {
		return nil, singleton.Localizer.ErrorT("incorrect password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pf.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user.Username = pf.NewUsername
	user.Password = string(hash)
	if err := singleton.DB.Save(&user).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}

// List user
// @Summary List user
// @Security BearerAuth
// @Schemes
// @Description List user
// @Tags auth required
// @Produce json
// @Success 200 {object} model.CommonResponse[[]model.User]
// @Router /user [get]
func listUser(c *gin.Context) ([]model.User, error) {
	var users []model.User
	if err := singleton.DB.Omit("password").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}

// Create user
// @Summary Create user
// @Security BearerAuth
// @Schemes
// @Description Create user
// @Tags auth required
// @Accept json
// @param request body model.UserForm true "User Request"
// @Produce json
// @Success 200 {object} model.CommonResponse[uint64]
// @Router /user [post]
func createUser(c *gin.Context) (uint64, error) {
	var uf model.UserForm
	if err := c.ShouldBindJSON(&uf); err != nil {
		return 0, err
	}

	if len(uf.Password) < 6 {
		return 0, singleton.Localizer.ErrorT("password length must be greater than 6")
	}
	if uf.Username == "" {
		return 0, singleton.Localizer.ErrorT("username can't be empty")
	}

	var u model.User
	u.Username = uf.Username
	u.Role = model.RoleMember

	hash, err := bcrypt.GenerateFromPassword([]byte(uf.Password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	u.Password = string(hash)

	if err := singleton.DB.Create(&u).Error; err != nil {
		return 0, err
	}

	singleton.OnUserUpdate(&u)
	return u.ID, nil
}

// Batch delete users
// @Summary Batch delete users
// @Security BearerAuth
// @Schemes
// @Description Batch delete users
// @Tags auth required
// @Accept json
// @param request body []uint true "id list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /batch-delete/user [post]
func batchDeleteUser(c *gin.Context) (any, error) {
	var ids []uint64
	if err := c.ShouldBindJSON(&ids); err != nil {
		return nil, err
	}
	auth := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	if slices.Contains(ids, auth.ID) {
		return nil, singleton.Localizer.ErrorT("can't delete yourself")
	}

	err := singleton.OnUserDelete(ids, newGormError)
	return nil, err
}
