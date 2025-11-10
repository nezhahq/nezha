package controller

import (
	"slices"
	"strconv"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
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
	var ob []model.Oauth2Bind
	if err := singleton.DB.Where("user_id = ?", auth.(*model.User).ID).Find(&ob).Error; err != nil {
		return nil, newGormError("%v", err)
	}
	var obMap = make(map[string]string)
	for _, v := range ob {
		obMap[v.Provider] = v.OpenID
	}
	return &model.Profile{
		User:       *auth.(*model.User),
		LoginIP:    c.GetString(model.CtxKeyRealIPStr),
		Oauth2Bind: obMap,
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

	var bindCount int64
	if err := singleton.DB.Model(&model.Oauth2Bind{}).Where("user_id = ?", auth.(*model.User).ID).Count(&bindCount).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	if pf.RejectPassword && bindCount < 1 {
		return nil, singleton.Localizer.ErrorT("you don't have any oauth2 bindings")
	}

	user.Username = pf.NewUsername
	user.Password = string(hash)
	user.RejectPassword = pf.RejectPassword
	if err := singleton.DB.Save(&user).Error; err != nil {
		return nil, newGormError("%v", err)
	}

	singleton.OnUserUpdate(&user)
	return nil, nil
}

// List user
// @Summary List user
// @Security BearerAuth
// @Schemes
// @Description List user
// @Tags admin required
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
// @Tags admin required
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
	if uf.Role > model.RoleMember {
		return 0, singleton.Localizer.ErrorT("invalid role")
	}

	var u model.User
	u.Username = uf.Username
	u.Role = uf.Role

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
// @Tags admin required
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

// List online users
// @Summary List online users
// @Security BearerAuth
// @Schemes
// @Description List online users
// @Tags auth required
// @Param limit query uint false "Page limit"
// @Param offset query uint false "Page offset"
// @Produce json
// @Success 200 {object} model.PaginatedResponse[[]model.OnlineUser, model.OnlineUser]
// @Router /online-user [get]
func listOnlineUser(c *gin.Context) (*model.Value[[]*model.OnlineUser], error) {
	var isAdmin bool
	u, ok := c.Get(model.CtxKeyAuthorizedUser)
	if ok {
		isAdmin = u.(*model.User).Role.IsAdmin()
	}
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit < 1 {
		limit = 25
	}

	offset, err := strconv.Atoi(c.Query("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}

	users := singleton.GetOnlineUsers(limit, offset)
	if !isAdmin {
		var newUsers []*model.OnlineUser
		for _, user := range users {
			newUsers = append(newUsers, &model.OnlineUser{
				UserID:      user.UserID,
				IP:          utils.IPDesensitize(user.IP),
				ConnectedAt: user.ConnectedAt,
			})
		}
		users = newUsers
	}

	return &model.Value[[]*model.OnlineUser]{
		Value: users,
		Pagination: model.Pagination{
			Offset: offset,
			Limit:  limit,
			Total:  int64(singleton.GetOnlineUserCount()),
		},
	}, nil
}

// Batch block online user
// @Summary Batch block online user
// @Security BearerAuth
// @Schemes
// @Description Batch block online user
// @Tags admin required
// @Accept json
// @Param request body []string true "block list"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /online-user/batch-block [post]
func batchBlockOnlineUser(c *gin.Context) (any, error) {
	var list []string
	if err := c.ShouldBindJSON(&list); err != nil {
		return nil, err
	}

	if err := singleton.BlockByIPs(utils.Unique(list)); err != nil {
		return nil, newGormError("%v", err)
	}

	return nil, nil
}
