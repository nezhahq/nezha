package controller

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/frontend"
	"github.com/nezhahq/nezha/service/singleton"
)

func registerFrontendRoutes(auth *gin.RouterGroup) {
	frontends := auth.Group("/frontend", restScopeMiddleware(model.ScopeAdminAll))
	frontends.GET("", adminHandler(listFrontends))
	frontends.GET("/releases", adminHandler(listFrontendReleases))
	frontends.POST("/install", adminHandler(installFrontend))
	frontends.POST("/activate", adminHandler(activateFrontend))
	frontends.POST("/remove", adminHandler(removeFrontend))
}

type frontendStatus struct {
	Templates     []model.FrontendTemplate `json:"templates"`
	UserTemplate  string                   `json:"user_template"`
	AdminTemplate string                   `json:"admin_template"`
}

type frontendInstallForm struct {
	Repository string `json:"repository" binding:"required"`
	Tag        string `json:"tag" binding:"required"`
	IsAdmin    bool   `json:"is_admin"`
}

type frontendActivateForm struct {
	Path    string `json:"path" binding:"required"`
	IsAdmin bool   `json:"is_admin"`
}

// List frontends
// @Summary List installed and built-in frontends
// @Security BearerAuth
// @Tags admin required
// @Produce json
// @Success 200 {object} model.CommonResponse[frontendStatus]
// @Router /frontend [get]
func listFrontends(c *gin.Context) (*frontendStatus, error) {
	user, admin := singleton.Conf.FrontendSelection()
	return &frontendStatus{Templates: singleton.Conf.AvailableFrontendTemplates(), UserTemplate: user, AdminTemplate: admin}, nil
}

// List frontend releases
// @Summary List releases containing dist.zip
// @Security BearerAuth
// @Tags admin required
// @Param repository query string true "Repository URL or repository API URL"
// @Param page query int false "Page number"
// @Produce json
// @Success 200 {object} model.CommonResponse[frontend.ReleasePage]
// @Router /frontend/releases [get]
func listFrontendReleases(c *gin.Context) (*frontend.ReleasePage, error) {
	page := 1
	if value := c.Query("page"); value != "" {
		var err error
		page, err = strconv.Atoi(value)
		if err != nil {
			return nil, errors.New("invalid release page")
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	return frontend.NewClient().Releases(ctx, c.Query("repository"), page)
}

// Install frontend
// @Summary Download and validate a frontend release without activating it
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @Param body body frontendInstallForm true "Frontend release"
// @Produce json
// @Success 200 {object} model.CommonResponse[model.FrontendTemplate]
// @Router /frontend/install [post]
func installFrontend(c *gin.Context) (*model.FrontendTemplate, error) {
	var form frontendInstallForm
	if err := c.ShouldBindJSON(&form); err != nil {
		return nil, err
	}
	if singleton.Conf.FrontendStore == nil {
		return nil, errors.New("frontend storage is not initialized")
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	return singleton.Conf.FrontendStore.Install(ctx, form.Repository, form.Tag, form.IsAdmin)
}

// Activate frontend
// @Summary Switch the user or administrator frontend without restarting
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @Param body body frontendActivateForm true "Installed frontend"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /frontend/activate [post]
func activateFrontend(c *gin.Context) (any, error) {
	var form frontendActivateForm
	if err := c.ShouldBindJSON(&form); err != nil {
		return nil, err
	}
	for _, template := range singleton.Conf.AvailableFrontendTemplates() {
		if template.Path != form.Path || template.IsAdmin != form.IsAdmin {
			continue
		}
		var user, admin *string
		if form.IsAdmin {
			admin = &form.Path
		} else {
			user = &form.Path
		}
		return nil, singleton.Conf.CommitFrontendSelection(user, admin, singleton.Conf.Save)
	}
	return nil, errors.New("frontend is not installed for the selected role")
}

// Remove frontend
// @Summary Remove an inactive custom frontend and its files
// @Security BearerAuth
// @Tags admin required
// @Accept json
// @Param body body frontendActivateForm true "Installed frontend"
// @Produce json
// @Success 200 {object} model.CommonResponse[any]
// @Router /frontend/remove [post]
func removeFrontend(c *gin.Context) (any, error) {
	var form frontendActivateForm
	if err := c.ShouldBindJSON(&form); err != nil {
		return nil, err
	}
	return nil, singleton.Conf.RemoveFrontend(form.Path, form.IsAdmin)
}
