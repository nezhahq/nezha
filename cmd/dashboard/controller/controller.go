package controller

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
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
	"github.com/nezhahq/nezha/pkg/utils"
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

	kickoffTransferGC()

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
	// /mcp — Model Context Protocol endpoint, authenticated by PAT only (闸 1 + 闸 2)。
	// 不放在 /api/v1 下：MCP client 配置 URL 更短，且 MCP transport 协议演进与 REST API
	// 解耦。鉴权一律走 apiTokenAuthMiddleware；不接受 JWT 以避免浏览器误触。
	// mcpOriginGuard 防止 DNS rebinding / 浏览器跨站调用。
	r.POST("/mcp", mcpOriginGuard(), apiTokenAuthMiddleware(), mcpEndpoint)
	// Streamable HTTP 规范要求：不实现 standalone SSE / session 时，GET / DELETE
	// 必须显式返回 405，让客户端走 POST-only 路径并跳过 session 终止流程。
	// 不显式注册时，Gin 会走 NoRoute → fallbackToFrontend，对 MCP 客户端是 HTML/404。
	r.GET("/mcp", mcpOriginGuard(), mcpMethodNotAllowed)
	r.DELETE("/mcp", mcpOriginGuard(), mcpMethodNotAllowed)
	r.GET("/mcp/download/:token", mcpOriginGuard(), transferDownloadHandler)
	r.POST("/mcp/upload/:token", mcpOriginGuard(), transferUploadHandler)
	registerAgentcompatRoutes(r)

	api := r.Group("api/v1")
	api.POST("/login", authMiddleware.LoginHandler)
	api.GET("/oauth2/:provider", commonHandler(oauth2redirect))

	fallbackAuthMw := fallbackAuthMiddleware(authMiddleware)
	fallbackAuth := api.Group("", fallbackAuthMw)
	fallbackAuth.GET("/setting", commonHandler(listConfig))
	fallbackAuth.GET("/oauth2/callback", commonHandler(oauth2callback(authMiddleware)))

	jwtMw := authMiddleware.MiddlewareFunc()
	patMw := apiTokenAuthMiddleware()
	authMw := jwtOrPATAuthMiddleware(patMw, jwtMw)
	// optional 路由：ForceAuth=true 走严格 PAT-or-JWT；ForceAuth=false 走
	// PAT-or-FallbackJWT，保证两种模式下 PAT 都会被解析，restScopeMiddleware
	// 才能按 scope 真实收口（否则匿名 PAT 请求会被当 guest，scope 失效）。
	optionalAuthMw := utils.IfOr(singleton.Conf.ForceAuth, authMw, patOrFallbackAuthMiddleware(patMw, fallbackAuthMw))

	optionalAuth := api.Group("", optionalAuthMw)
	optionalAuth.GET("/ws/server", restScopeMiddleware(model.ScopeInventoryRead), commonHandler(serverStream))
	optionalAuth.GET("/server-group", restScopeMiddleware(model.ScopeInventoryRead), commonHandler(listServerGroup))

	optionalAuth.GET("/service", restScopeMiddleware(model.ScopeServiceRead), commonHandler(showService))
	optionalAuth.GET("/service/server", restScopeMiddleware(model.ScopeServiceRead), commonHandler(listServerWithServices))
	optionalAuth.GET("/service/:id/history", restScopeMiddleware(model.ScopeServiceRead), commonHandler(getServiceHistory))
	optionalAuth.GET("/server/:id/service", restScopeMiddleware(model.ScopeServiceRead), commonHandler(listServerServices))
	optionalAuth.GET("/server/:id/metrics", restScopeMiddleware(model.ScopeServerRead), commonHandler(getServerMetrics))

	// CSRF middleware applies group-wide. Safe methods short-circuit and
	// PAT bearer requests bypass — so the only callers gated are
	// cookie-JWT POST/PATCH/PUT/DELETE, which is exactly the H6 surface.
	auth := api.Group("", authMw, csrfMiddleware())

	// 「自我管理」类端点 — 显式禁止 PAT 访问（避免 PAT 自我提权链）。
	patForbidden := restPATForbiddenMiddleware()
	auth.POST("/refresh-token", patForbidden, authMiddleware.RefreshHandler)
	auth.GET("/profile", patForbidden, commonHandler(getProfile))
	auth.POST("/profile", patForbidden, commonHandler(updateProfile))
	auth.POST("/oauth2/:provider/unbind", patForbidden, commonHandler(unbindOauth2))
	auth.GET("/api-tokens", patForbidden, commonHandler(listAPITokens))
	auth.POST("/api-tokens", patForbidden, commonHandler(createAPIToken))
	auth.DELETE("/api-tokens/:id", patForbidden, commonHandler(deleteAPIToken))

	// 资源族划分：
	//   - nezha:inventory:* —— 对“服务器台账”的枚举与删除（列出 server / server-group、
	//     删除 server / server-group）。这是管理后台清单管理动作。
	//   - nezha:server:*    —— 对已知 server 的运行态操作（exec、文件读写、编辑配置、
	//     force-update、batch-move）。
	auth.POST("/terminal", restScopeMiddleware(model.ScopeServerExec), commonHandler(createTerminal))
	auth.GET("/ws/terminal/:id", restScopeMiddleware(model.ScopeServerExec), commonHandler(terminalStream))
	auth.POST("/file", restScopeAllOf(model.ScopeServerRead, model.ScopeServerWrite, model.ScopeServerDelete), commonHandler(createFM))
	auth.GET("/ws/file/:id", restScopeAllOf(model.ScopeServerRead, model.ScopeServerWrite, model.ScopeServerDelete), commonHandler(fmStream))
	auth.GET("/server", restScopeMiddleware(model.ScopeInventoryRead), listHandler(listServer))
	auth.PATCH("/server/:id", restScopeMiddleware(model.ScopeServerWrite), commonHandler(updateServer))
	auth.GET("/server/config/:id", restScopeMiddleware(serverConfigSensitiveScope()), commonHandler(getServerConfig))
	auth.POST("/server/config", restScopeMiddleware(model.ScopeServerWrite), commonHandler(setServerConfig))
	auth.POST("/batch-delete/server", restScopeMiddleware(model.ScopeInventoryDelete), commonHandler(batchDeleteServer))
	auth.POST("/batch-move/server", restScopeMiddleware(model.ScopeServerWrite), commonHandler(batchMoveServer))
	auth.POST("/force-update/server", restScopeMiddleware(model.ScopeServerWrite), commonHandler(forceUpdateServer))
	auth.POST("/server-group", restScopeMiddleware(model.ScopeServerWrite), commonHandler(createServerGroup))
	auth.PATCH("/server-group/:id", restScopeMiddleware(model.ScopeServerWrite), commonHandler(updateServerGroup))
	auth.POST("/batch-delete/server-group", restScopeMiddleware(model.ScopeInventoryDelete), commonHandler(batchDeleteServerGroup))

	// transfer — 严格使用 nezha:transfer 资源族 scope（read/write/delete）。
	// 注意：曾经计划让 nezha:server:read 兼听只读 transfer，但 restScopeMiddleware
	// / APIToken.HasScope 不做 server↔transfer 别名展开，前端 SCOPE_OPTIONS 也已经
	// 单独暴露 nezha:transfer:read，所以这里维持精确匹配语义。
	auth.GET("/transfer", restScopeMiddleware(model.ScopeTransferRead), listHandler(listServerTransfer))
	auth.POST("/transfer/:id/cancel", restScopeMiddleware(model.ScopeTransferWrite), commonHandler(cancelServerTransfer))
	auth.POST("/transfer/:id/retry", restScopeMiddleware(model.ScopeTransferWrite), commonHandler(retryServerTransfer))
	auth.GET("/ws/transfer", restScopeMiddleware(model.ScopeTransferRead), commonHandler(transferStream))

	// service monitor
	auth.GET("/service/list", restScopeMiddleware(model.ScopeServiceRead), listHandler(listService))
	auth.POST("/service", restScopeMiddleware(model.ScopeServiceWrite), commonHandler(createService))
	auth.PATCH("/service/:id", restScopeMiddleware(model.ScopeServiceWrite), commonHandler(updateService))
	auth.POST("/batch-delete/service", restScopeMiddleware(model.ScopeServiceDelete), commonHandler(batchDeleteService))

	auth.GET("/notification-group", restScopeMiddleware(model.ScopeNotificationGroupRead), commonHandler(listNotificationGroup))
	auth.POST("/notification-group", restScopeMiddleware(model.ScopeNotificationGroupWrite), commonHandler(createNotificationGroup))
	auth.PATCH("/notification-group/:id", restScopeMiddleware(model.ScopeNotificationGroupWrite), commonHandler(updateNotificationGroup))
	auth.POST("/batch-delete/notification-group", restScopeMiddleware(model.ScopeNotificationGroupDelete), commonHandler(batchDeleteNotificationGroup))

	auth.GET("/notification", restScopeMiddleware(model.ScopeNotificationRead), listHandler(listNotification))
	auth.POST("/notification", restScopeMiddleware(model.ScopeNotificationWrite), commonHandler(createNotification))
	auth.PATCH("/notification/:id", restScopeMiddleware(model.ScopeNotificationWrite), commonHandler(updateNotification))
	auth.POST("/batch-delete/notification", restScopeMiddleware(model.ScopeNotificationDelete), commonHandler(batchDeleteNotification))

	auth.GET("/alert-rule", restScopeMiddleware(model.ScopeAlertRuleRead), listHandler(listAlertRule))
	auth.POST("/alert-rule", restScopeMiddleware(model.ScopeAlertRuleWrite), commonHandler(createAlertRule))
	auth.PATCH("/alert-rule/:id", restScopeMiddleware(model.ScopeAlertRuleWrite), commonHandler(updateAlertRule))
	auth.POST("/batch-delete/alert-rule", restScopeMiddleware(model.ScopeAlertRuleDelete), commonHandler(batchDeleteAlertRule))

	auth.GET("/cron", restScopeMiddleware(model.ScopeCronRead), listHandler(listCron))
	auth.POST("/cron", restScopeMiddleware(model.ScopeCronWrite), commonHandler(createCron))
	auth.PATCH("/cron/:id", restScopeMiddleware(model.ScopeCronWrite), commonHandler(updateCron))
	auth.POST("/cron/:id/manual", restScopeMiddleware(model.ScopeCronExec), commonHandler(manualTriggerCron))
	auth.POST("/batch-delete/cron", restScopeMiddleware(model.ScopeCronDelete), commonHandler(batchDeleteCron))

	auth.GET("/ddns", restScopeMiddleware(model.ScopeDDNSRead), listHandler(listDDNS))
	auth.GET("/ddns/providers", restScopeMiddleware(model.ScopeDDNSRead), commonHandler(listProviders))
	auth.POST("/ddns", restScopeMiddleware(model.ScopeDDNSWrite), commonHandler(createDDNS))
	auth.PATCH("/ddns/:id", restScopeMiddleware(model.ScopeDDNSWrite), commonHandler(updateDDNS))
	auth.POST("/batch-delete/ddns", restScopeMiddleware(model.ScopeDDNSDelete), commonHandler(batchDeleteDDNS))

	auth.GET("/nat", restScopeMiddleware(model.ScopeNATRead), listHandler(listNAT))
	auth.POST("/nat", restScopeMiddleware(model.ScopeNATWrite), commonHandler(createNAT))
	auth.PATCH("/nat/:id", restScopeMiddleware(model.ScopeNATWrite), commonHandler(updateNAT))
	auth.POST("/batch-delete/nat", restScopeMiddleware(model.ScopeNATDelete), commonHandler(batchDeleteNAT))

	// 管理员资源 — 仅 nezha:* / nezha:admin:* 持有者可调（adminHandler 进一步校验 user.Role）。
	auth.GET("/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(listUser))
	auth.POST("/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(createUser))
	auth.POST("/batch-delete/user", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchDeleteUser))
	auth.GET("/waf", restScopeMiddleware(model.ScopeAdminAll), pAdminHandler(listBlockedAddress))
	auth.POST("/batch-delete/waf", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchDeleteBlockedAddress))
	auth.GET("/online-user", restScopeMiddleware(model.ScopeAdminAll), pAdminHandler(listOnlineUser))
	auth.POST("/online-user/batch-block", restScopeMiddleware(model.ScopeAdminAll), adminHandler(batchBlockOnlineUser))
	auth.PATCH("/setting", restScopeMiddleware(model.ScopeAdminAll), adminHandler(updateConfig))
	auth.POST("/maintenance", restScopeMiddleware(model.ScopeAdminAll), adminHandler(runMaintenance))

	// 内置 LLM Chat：admin-only。llmChat 是流式 SSE 端点，写过 SSE 头后
	// 不能 c.JSON，故不走 adminHandler 包装，handler 内部自行做 Role 校验。
	// llmTest 是同步连通性测试，正常 JSON 响应，可用 adminHandler。
	auth.POST("/llm/chat", restScopeMiddleware(model.ScopeAdminAll), llmChat)
	auth.POST("/llm/test", restScopeMiddleware(model.ScopeAdminAll), adminHandler(llmTest))

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
	a   []any
}

func newGormError(format string, args ...any) error {
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
	a   []any
}

func newWsError(format string, args ...any) error {
	return &wsError{
		msg: format,
		a:   args,
	}
}

func (we *wsError) Error() string {
	return fmt.Sprintf(we.msg, we.a...)
}

var errNoop = errors.New("wrote")

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
		if !user.Role.IsAdmin() {
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
		if !errors.Is(err, errNoop) {
			c.JSON(http.StatusOK, newErrorResponse(err))
		}
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

		filtered := filter(c, data)
		c.JSON(http.StatusOK, model.CommonResponse[S]{Success: true, Data: model.SearchByIDCtx(c, filtered)})
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

func pAdminHandler[S ~[]E, E any](handler pHandlerFunc[S, E]) func(*gin.Context) {
	return func(c *gin.Context) {
		auth, ok := c.Get(model.CtxKeyAuthorizedUser)
		if !ok {
			c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("unauthorized")))
			return
		}
		user := *auth.(*model.User)
		if !user.Role.IsAdmin() {
			c.JSON(http.StatusOK, newErrorResponse(singleton.Localizer.ErrorT("permission denied")))
			return
		}

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
	serveFile := func(c *gin.Context, name string, file fs.File, customStatusCode int) bool {
		defer file.Close()
		fileStat, err := file.Stat()
		if err != nil {
			return false
		}
		if fileStat.IsDir() {
			return false
		}
		readSeeker, ok := file.(io.ReadSeeker)
		if !ok {
			return false
		}
		http.ServeContent(utils.NewGinCustomWriter(c, customStatusCode), c.Request, name, fileStat.ModTime(), readSeeker)
		return true
	}

	checkLocalFileOrFs := func(c *gin.Context, frontendFS fs.FS, templateRoot, filePath string, customStatusCode int) bool {
		if filePath != "" {
			localRoot, err := os.OpenRoot(templateRoot)
			if err == nil {
				defer localRoot.Close()
				// URL paths must stay inside the selected template root; never join them against the process cwd.
				if file, err := localRoot.Open(filePath); err == nil && serveFile(c, filePath, file, customStatusCode) {
					return true
				}
			}
		}

		if !fs.ValidPath(filePath) {
			return false
		}
		templateFS, err := fs.Sub(frontendFS, templateRoot)
		if err != nil {
			return false
		}
		file, err := templateFS.Open(filePath)
		if err != nil {
			return false
		}
		if serveFile(c, filePath, file, customStatusCode) {
			return true
		}
		return false
	}

	frontendPageUrlRegistry := []*regexp.Regexp{
		// official user frontend
		regexp.MustCompile(`^/$`),
		regexp.MustCompile(`^/server/\d*$`),
		// backend frontend
		regexp.MustCompile(`^/dashboard/$`),
		regexp.MustCompile(`^/dashboard/login$`),
		regexp.MustCompile(`^/dashboard/service$`),
		regexp.MustCompile(`^/dashboard/cron$`),
		regexp.MustCompile(`^/dashboard/notification$`),
		regexp.MustCompile(`^/dashboard/alert-rule$`),
		regexp.MustCompile(`^/dashboard/ddns$`),
		regexp.MustCompile(`^/dashboard/nat$`),
		regexp.MustCompile(`^/dashboard/server-group$`),
		regexp.MustCompile(`^/dashboard/notification-group$`),
		regexp.MustCompile(`^/dashboard/profile$`),
		regexp.MustCompile(`^/dashboard/settings$`),
		regexp.MustCompile(`^/dashboard/settings/user$`),
		regexp.MustCompile(`^/dashboard/settings/online-user$`),
		regexp.MustCompile(`^/dashboard/settings/waf$`),
		regexp.MustCompile(`^/dashboard/settings/api-tokens$`),
		// 注意：这里的白名单决定哪些 URL 走 index.html fallback；漏一条就会把
		// 直接刷新该页面变成 404（HTTP 状态码层面，body 仍是 index.html，所以
		// 浏览器内 SPA 看起来正常，但 monitoring / 链接预览会以为站点挂了）。
		// 新增前端路由时必须在 admin-frontend/src/main.tsx 与这里同步加。
		regexp.MustCompile(`^/dashboard/transfer$`),
	}

	getFallbackStatusCode := func(path string) int {
		for _, reg := range frontendPageUrlRegistry {
			if reg.MatchString(path) {
				return http.StatusOK
			}
		}
		return http.StatusNotFound
	}

	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
			return
		}

		// redirect for /dashboard to /dashboard/
		if c.Request.URL.Path == "/dashboard" {
			c.Redirect(http.StatusMovedPermanently, "/dashboard/")
			return
		}

		fallbackStatusCode := getFallbackStatusCode(c.Request.URL.Path)
		// Only /dashboard/ belongs to the admin frontend; /dashboard.. must not be trimmed into ../.
		if strings.HasPrefix(c.Request.URL.Path, "/dashboard/") {
			stripPath := strings.TrimPrefix(c.Request.URL.Path, "/dashboard/")
			if checkLocalFileOrFs(c, frontendDist, singleton.Conf.AdminTemplate, stripPath, http.StatusOK) {
				return
			}
			if !checkLocalFileOrFs(c, frontendDist, singleton.Conf.AdminTemplate, "index.html", fallbackStatusCode) {
				c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
			}
			return
		}
		stripPath := strings.TrimPrefix(c.Request.URL.Path, "/")
		if checkLocalFileOrFs(c, frontendDist, singleton.Conf.UserTemplate, stripPath, http.StatusOK) {
			return
		}
		if !checkLocalFileOrFs(c, frontendDist, singleton.Conf.UserTemplate, "index.html", fallbackStatusCode) {
			c.JSON(http.StatusNotFound, newErrorResponse(errors.New("404 Not Found")))
		}
	}
}
