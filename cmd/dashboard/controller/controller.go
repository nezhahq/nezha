package controller

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"github.com/hashicorp/go-uuid"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	docs "github.com/naiba/nezha/cmd/dashboard/docs"
	"github.com/naiba/nezha/model"
	"github.com/naiba/nezha/pkg/utils"
	"github.com/naiba/nezha/proto"
	"github.com/naiba/nezha/service/rpc"
	"github.com/naiba/nezha/service/singleton"
)

func ServeWeb() *http.Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	docs.SwaggerInfo.BasePath = "/api/v1"
	if singleton.Conf.Debug {
		gin.SetMode(gin.DebugMode)
		pprof.Register(r)
	}
	r.Use(natGateway)
	if singleton.Conf.Debug {
		r.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerfiles.Handler))
	}

	r.Use(recordPath)
	routers(r)

	return &http.Server{
		ReadHeaderTimeout: time.Second * 5,
		Handler:           r,
	}
}

func routers(r *gin.Engine) {
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
	optionalAuth.GET("/ws/server", commonHandler[any](serverStream))
	optionalAuth.GET("/server-group", commonHandler[[]model.ServerGroup](listServerGroup))

	auth := api.Group("", authMiddleware.MiddlewareFunc())
	auth.GET("/refresh_token", authMiddleware.RefreshHandler)
	auth.PATCH("/server/:id", commonHandler[any](editServer))

	auth.GET("/ddns", commonHandler[[]model.DDNSProfile](listDDNS))
	auth.POST("/ddns", commonHandler[any](newDDNS))
	auth.PATCH("/ddns/:id", commonHandler[any](editDDNS))

	api.POST("/batch-delete/server", commonHandler[any](batchDeleteServer))
	api.POST("/batch-delete/ddns", commonHandler[any](batchDeleteDDNS))

	// 通用页面
	// cp := commonPage{r: r}
	// cp.serve()
	// // 会员页面
	// mp := &memberPage{r}
	// mp.serve()
	// // API
	// external := api.Group("api")
	// {
	// 	ma := &memberAPI{external}
	// 	ma.serve()
	// }
}

func natGateway(c *gin.Context) {
	natConfig := singleton.GetNATConfigByDomain(c.Request.Host)
	if natConfig == nil {
		return
	}

	singleton.ServerLock.RLock()
	server := singleton.ServerList[natConfig.ServerID]
	singleton.ServerLock.RUnlock()
	if server == nil || server.TaskStream == nil {
		c.Writer.WriteString("server not found or not connected")
		c.Abort()
		return
	}

	streamId, err := uuid.GenerateUUID()
	if err != nil {
		c.Writer.WriteString(fmt.Sprintf("stream id error: %v", err))
		c.Abort()
		return
	}

	rpc.NezhaHandlerSingleton.CreateStream(streamId)
	defer rpc.NezhaHandlerSingleton.CloseStream(streamId)

	taskData, err := utils.Json.Marshal(model.TaskNAT{
		StreamID: streamId,
		Host:     natConfig.Host,
	})
	if err != nil {
		c.Writer.WriteString(fmt.Sprintf("task data error: %v", err))
		c.Abort()
		return
	}

	if err := server.TaskStream.Send(&proto.Task{
		Type: model.TaskTypeNAT,
		Data: string(taskData),
	}); err != nil {
		c.Writer.WriteString(fmt.Sprintf("send task error: %v", err))
		c.Abort()
		return
	}

	w, err := utils.NewRequestWrapper(c.Request, c.Writer)
	if err != nil {
		c.Writer.WriteString(fmt.Sprintf("request wrapper error: %v", err))
		c.Abort()
		return
	}

	if err := rpc.NezhaHandlerSingleton.UserConnected(streamId, w); err != nil {
		c.Writer.WriteString(fmt.Sprintf("user connected error: %v", err))
		c.Abort()
		return
	}

	rpc.NezhaHandlerSingleton.StartStream(streamId, time.Second*10)
	c.Abort()
}

func recordPath(c *gin.Context) {
	url := c.Request.URL.String()
	for _, p := range c.Params {
		url = strings.Replace(url, p.Value, ":"+p.Key, 1)
	}
	c.Set("MatchedPath", url)
}

func newErrorResponse[T any](err error) model.CommonResponse[T] {
	return model.CommonResponse[T]{
		Success: false,
		Error:   err.Error(),
	}
}

type handlerFunc func(c *gin.Context) error

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

func commonHandler[T any](handler handlerFunc) func(*gin.Context) {
	return func(c *gin.Context) {
		if err := handler(c); err != nil {
			if _, ok := err.(*gormError); ok {
				log.Printf("NEZHA>> gorm error: %v", err)
				c.JSON(http.StatusOK, newErrorResponse[T](errors.New("database error")))
				return
			} else {
				c.JSON(http.StatusOK, newErrorResponse[T](err))
				return
			}
		}
	}
}
