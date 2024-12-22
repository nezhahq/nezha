package controller

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	docs "github.com/nezhahq/nezha/cmd/dashboard/docs"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/service/singleton"
)

func ServeWeb(frontendDist fs.FS) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	if singleton.Conf.Debug {
		gin.SetMode(gin.DebugMode)
		pprof.Register(r)
	}
	if singleton.Conf.Debug {
		log.Printf("NEZHA>> Swagger(%s) UI available at http://localhost:%d/swagger/index.html", docs.SwaggerInfo.Version, singleton.Conf.ListenPort)
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))
	}

	r.Use(waf.RealIp)
	r.Use(waf.Waf)
	r.Use(recordPath)

	routers(r, frontendDist)

	return r
}

func routers(r *gin.Engine, frontendDist fs.FS) {
	authMiddleware, err := jwt.New(initParams())
	if err != nil {
		log.Fatal("JWT Error:" + err.Error())
	}
	if err := authMiddleware.MiddlewareInit(); err != nil {
		log.Fatal("authMiddleware.MiddlewareInit Error:" + err.Error())
	}
	api := r.Group("api/v1")
	api.POST("/login", authMiddleware.LoginHandler)

	optionalAuth := api.Group("", optionalAuthMiddleware(authMiddleware))
	optionalAuth.GET("/ws/server", commonHandler(serverStream))
	optionalAuth.GET("/server-group", commonHandler(listServerGroup))

	optionalAuth.GET("/service", commonHandler(showService))
	optionalAuth.GET("/service/:id", commonHandler(listServiceHistory))
	optionalAuth.GET("/service/server", commonHandler(listServerWithServices))

	optionalAuth.GET("/setting", commonHandler(listConfig))

	auth := api.Group("", authMiddleware.MiddlewareFunc())

	auth.GET("/refresh-token", authMiddleware.RefreshHandler)

	auth.POST("/terminal", commonHandler(createTerminal))
	auth.GET("/ws/terminal/:id", commonHandler(terminalStream))

	auth.GET("/file", commonHandler(createFM))
	auth.GET("/ws/file/:id", commonHandler(fmStream))

	auth.GET("/profile", commonHandler(getProfile))
	auth.POST("/profile", commonHandler(updateProfile))
	auth.GET("/user", adminHandler(listUser))
	auth.POST("/user", adminHandler(createUser))
	auth.POST("/batch-delete/user", adminHandler(batchDeleteUser))

	auth.GET("/service/list", listHandler(listService))
	auth.POST("/service", commonHandler(createService))
	auth.PATCH("/service/:id", commonHandler(updateService))
	auth.POST("/batch-delete/service", commonHandler(batchDeleteService))

	auth.POST("/server-group", commonHandler(createServerGroup))
	auth.PATCH("/server-group/:id", commonHandler(updateServerGroup))
	auth.POST("/batch-delete/server-group", commonHandler(batchDeleteServerGroup))

	auth.GET("/notification-group", commonHandler(listNotificationGroup))
	auth.POST("/notification-group", commonHandler(createNotificationGroup))
	auth.PATCH("/notification-group/:id", commonHandler(updateNotificationGroup))
	auth.POST("/batch-delete/notification-group", commonHandler(batchDeleteNotificationGroup))

	auth.GET("/server", listHandler(listServer))
	auth.PATCH("/server/:id", commonHandler(updateServer))
	auth.POST("/batch-delete/server", commonHandler(batchDeleteServer))
	auth.POST("/force-update/server", commonHandler(forceUpdateServer))

	auth.GET("/notification", listHandler(listNotification))
	auth.POST("/notification", commonHandler(createNotification))
	auth.PATCH("/notification/:id", commonHandler(updateNotification))
	auth.POST("/batch-delete/notification", commonHandler(batchDeleteNotification))

	auth.GET("/alert-rule", listHandler(listAlertRule))
	auth.POST("/alert-rule", commonHandler(createAlertRule))
	auth.PATCH("/alert-rule/:id", commonHandler(updateAlertRule))
	auth.POST("/batch-delete/alert-rule", commonHandler(batchDeleteAlertRule))

	auth.GET("/cron", listHandler(listCron))
	auth.POST("/cron", commonHandler(createCron))
	auth.PATCH("/cron/:id", commonHandler(updateCron))
	auth.GET("/cron/:id/manual", commonHandler(manualTriggerCron))
	auth.POST("/batch-delete/cron", commonHandler(batchDeleteCron))

	auth.GET("/ddns", listHandler(listDDNS))
	auth.GET("/ddns/providers", commonHandler(listProviders))
	auth.POST("/ddns", commonHandler(createDDNS))
	auth.PATCH("/ddns/:id", commonHandler(updateDDNS))
	auth.POST("/batch-delete/ddns", commonHandler(batchDeleteDDNS))

	auth.GET("/nat", listHandler(listNAT))
	auth.POST("/nat", commonHandler(createNAT))
	auth.PATCH("/nat/:id", commonHandler(updateNAT))
	auth.POST("/batch-delete/nat", commonHandler(batchDeleteNAT))

	auth.GET("/waf", pCommonHandler(listBlockedAddress))
	auth.POST("/batch-delete/waf", adminHandler(batchDeleteBlockedAddress))

	auth.GET("/online-user", pCommonHandler(listOnlineUser))
	auth.POST("/online-user/batch-block", adminHandler(batchBlockOnlineUser))

	auth.PATCH("/setting", adminHandler(updateConfig))

	r.NoRoute(fallbackToFrontend(frontendDist))
}

func recordPath(c *gin.Context) {
	url := c.Request.URL.String()
	for _, p := range c.Params {
		url = strings.Replace(url, p.Value, ":"+p.Key, 1)
	}
	c.Set("MatchedPath", url)
}

func newErrorResponse(err error) model.CommonResponse[any] {
	return model.CommonResponse[any]{
		Success: false,
		Error:   err.Error(),
	}
}

type handlerFunc[T any] func(c *gin.Context) (T, error)
type pHandlerFunc[S ~[]E, E any] func(c *gin.Context) (*model.Value[S], error)

// There are many error types in gorm, so create a custom type to represent all
// gorm errors here instead
type gormError struct {
	msg string
	a   []interface{}
}

func newGormError(format string, args ...interface{}) error {
	return &gormError{
		msg: format,
		a:   args,
	}
}

func (ge *gormError) Error() string {
	return fmt.Sprintf(ge.msg, ge.a...)
}

type wsError struct {
	msg string
	a   []interface{}
}

func newWsError(format string, args ...interface{}) error {
	return &wsError{
		msg: format,
		a:   args,
	}
}

func (we *wsError) Error() string {
	return fmt.Sprintf(we.msg, we.a...)
}

func commonHandler[T any](handler handlerFunc[T]) func(*gin.Context) {
	return func(c *gin.Context) {
		handle(c, handler)
	}
}

func adminHandler[T any](handler handlerFunc[T]) func(*gin.Context) {
	return func(c *gin.Context) {
		auth, ok := c.Get(model.CtxKeyAuthorizedUser)
		if !ok {
			c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("unauthorized")))
			return
		}

		user := *auth.(*model.User)
		if user.Role != model.RoleAdmin {
			c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("permission denied")))
			return
		}

		handle(c, handler)
	}
}

func handle[T any](c *gin.Context, handler handlerFunc[T]) {
	data, err := handler(c)
	if err == nil {
		c.JSON(http.StatusOK, model.CommonResponse[T]{Success: true, Data: data})
		return
	}
	switch err.(type) {
	case *gormError:
		log.Printf("NEZHA>> gorm error: %v", err)
		c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("database error")))
		return
	case *wsError:
		// Connection is upgraded to WebSocket, so c.Writer is no longer usable
		if msg := err.Error(); msg != "" {
			log.Printf("NEZHA>> websocket error: %v", err)
		}
		return
	default:
		c.JSON(http.StatusOK, newErrorResponse(err))
		return
	}
}

func listHandler[S ~[]E, E model.CommonInterface](handler handlerFunc[S]) func(*gin.Context) {
	return func(c *gin.Context) {
		data, err := handler(c)
		if err != nil {
			c.JSON(http.StatusOK, newErrorResponse(err))
			return
		}

		c.JSON(http.StatusOK, model.CommonResponse[S]{Success: true, Data: filter(c, data)})
	}
}

func pCommonHandler[S ~[]E, E any](handler pHandlerFunc[S, E]) func(*gin.Context) {
	return func(c *gin.Context) {
		data, err := handler(c)
		if err != nil {
			c.JSON(http.StatusOK, newErrorResponse(err))
			return
		}

		c.JSON(http.StatusOK, model.PaginatedResponse[S, E]{Success: true, Data: data})
	}
}

func filter[S ~[]E, E model.CommonInterface](ctx *gin.Context, s S) S {
	return slices.DeleteFunc(s, func(e E) bool {
		return !e.HasPermission(ctx)
	})
}

func getUid(c *gin.Context) uint64 {
	user, _ := c.MustGet(model.CtxKeyAuthorizedUser).(*model.User)
	return user.ID
}

func fallbackToFrontend(frontendDist fs.FS) func(*gin.Context) {
	checkLocalFileOrFs := func(c *gin.Context, fs fs.FS, path string) bool {
		if _, err := os.Stat(path); err == nil {
			c.File(path)
			return true
		}
		f, err := fs.Open(path)
		if err != nil {
			return false
		}
		defer f.Close()
		fileStat, err := f.Stat()
		if err != nil {
			return false
		}
		if fileStat.IsDir() {
			return false
		}
		http.ServeContent(c.Writer, c.Request, path, fileStat.ModTime(), f.(io.ReadSeeker))
		return true
	}
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.JSON(http.StatusOK, newErrorResponse(errors.New("404 Not Found")))
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/dashboard") {
			stripPath := strings.TrimPrefix(c.Request.URL.Path, "/dashboard")
			localFilePath := path.Join(singleton.Conf.AdminTemplate, stripPath)
			if checkLocalFileOrFs(c, frontendDist, localFilePath) {
				return
			}
			if !checkLocalFileOrFs(c, frontendDist, singleton.Conf.AdminTemplate+"/index.html") {
				c.JSON(http.StatusOK, newErrorResponse(errors.New("404 Not Found")))
			}
			return
		}
		localFilePath := path.Join(singleton.Conf.UserTemplate, c.Request.URL.Path)
		if checkLocalFileOrFs(c, frontendDist, localFilePath) {
			return
		}
		if !checkLocalFileOrFs(c, frontendDist, singleton.Conf.UserTemplate+"/index.html") {
			c.JSON(http.StatusOK, newErrorResponse(errors.New("404 Not Found")))
		}
	}
}
