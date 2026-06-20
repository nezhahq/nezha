package controller

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/gin-contrib/pprof"
	"github.com/gin-gonic/gin"
	"github.com/hashicorp/go-uuid"
	"github.com/nicksnyder/go-i18n/v2/i18n"

	"github.com/naiba/nezha/model"
	"github.com/naiba/nezha/pkg/mygin"
	"github.com/naiba/nezha/pkg/utils"
	"github.com/naiba/nezha/proto"
	"github.com/naiba/nezha/resource"
	"github.com/naiba/nezha/service/rpc"
	"github.com/naiba/nezha/service/singleton"
)

func ServeWeb(port uint) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	if singleton.Conf.Debug {
		gin.SetMode(gin.DebugMode)
		pprof.Register(r)
	}
	r.Use(natGateway)
	r.Use(rejectUnsafePath)
	tmpl := template.New("").Funcs(funcMap)
	var err error
	tmpl, err = tmpl.ParseFS(resource.TemplateFS, "template/**/*.html")
	if err != nil {
		panic(err)
	}
	tmpl = loadThirdPartyTemplates(tmpl)
	r.SetHTMLTemplate(tmpl)
	r.Use(mygin.RecordPath)
	r.StaticFS("/static", http.FS(resource.StaticFS))
	routers(r)
	page404 := func(c *gin.Context) {
		mygin.ShowErrorPage(c, mygin.ErrInfo{
			Code:  http.StatusNotFound,
			Title: "该页面不存在",
			Msg:   "该页面内容可能已着陆火星",
			Link:  "/",
			Btn:   "返回首页",
		}, true)
	}
	r.NoRoute(page404)
	r.NoMethod(page404)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		ReadHeaderTimeout: time.Second * 5,
		Handler:           r,
	}
	return srv
}

func routers(r *gin.Engine) {
	// 通用页面
	cp := commonPage{r: r}
	cp.serve()
	// 游客页面
	gp := guestPage{r}
	gp.serve()
	// 会员页面
	mp := &memberPage{r}
	mp.serve()
	// API
	api := r.Group("api")
	{
		ma := &memberAPI{api}
		ma.serve()
	}
}

func loadThirdPartyTemplates(tmpl *template.Template) *template.Template {
	ret := tmpl
	themes, err := os.ReadDir("resource/template")
	if err != nil {
		log.Printf("NEZHA>> Error reading themes folder: %v", err)
		return ret
	}
	for _, theme := range themes {
		if !theme.IsDir() {
			continue
		}

		themeDir := theme.Name()
		if themeDir == "theme-custom" {
			// for backward compatibility
			// note: will remove this in future versions
			ret = loadTemplates(ret, themeDir)
			continue
		}

		if strings.HasPrefix(themeDir, "dashboard-") {
			// load dashboard templates, ignore desc file
			ret = loadTemplates(ret, themeDir)
			continue
		}

		if !strings.HasPrefix(themeDir, "theme-") {
			log.Printf("NEZHA>> Invalid theme name: %s", themeDir)
			continue
		}

		descPath := filepath.Join("resource", "template", themeDir, "theme.json")
		desc, err := os.ReadFile(filepath.Clean(descPath))
		if err != nil {
			log.Printf("NEZHA>> Error opening %s config: %v", themeDir, err)
			continue
		}

		themeName, err := utils.GjsonGet(desc, "name")
		if err != nil {
			log.Printf("NEZHA>> Error opening %s config: not a valid description file", theme.Name())
			continue
		}

		// load templates
		ret = loadTemplates(ret, themeDir)

		themeKey := strings.TrimPrefix(themeDir, "theme-")
		model.Themes[themeKey] = themeName.String()
	}

	return ret
}

func loadTemplates(tmpl *template.Template, themeDir string) *template.Template {
	// load templates
	templatePath := filepath.Join("resource", "template", themeDir, "*.html")
	t, err := tmpl.ParseGlob(templatePath)
	if err != nil {
		log.Printf("NEZHA>> Error parsing templates %s: %v", themeDir, err)
		return tmpl
	}

	return t
}

func rejectUnsafePath(c *gin.Context) {
	requestPath, err := url.PathUnescape(c.Request.URL.EscapedPath())
	if err != nil || strings.Contains(requestPath, "..") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
}

var funcMap = template.FuncMap{
	"tr": func(id string, dataAndCount ...interface{}) string {
		conf := i18n.LocalizeConfig{
			MessageID: id,
		}
		if len(dataAndCount) > 0 {
			conf.TemplateData = dataAndCount[0]
		}
		if len(dataAndCount) > 1 {
			conf.PluralCount = dataAndCount[1]
		}
		return singleton.Localizer.MustLocalize(&conf)
	},
	"toValMap": func(val interface{}) map[string]interface{} {
		return map[string]interface{}{
			"Value": val,
		}
	},
	"tf": func(t time.Time) string {
		return t.In(singleton.Loc).Format("01/02/2006 15:04:05")
	},
	"len": func(slice []interface{}) string {
		return strconv.Itoa(len(slice))
	},
	"safe": func(s string) template.HTML {
		return template.HTML(s) // #nosec
	},
	"tag": func(s string) template.HTML {
		return template.HTML(`<` + s + `>`) // #nosec
	},
	"stf": func(s uint64) string {
		return time.Unix(int64(s), 0).In(singleton.Loc).Format("01/02/2006 15:04")
	},
	"sf": func(duration uint64) string {
		return time.Duration(time.Duration(duration) * time.Second).String()
	},
	"sft": func(future time.Time) string {
		return time.Until(future).Round(time.Second).String()
	},
	"bf": func(b uint64) string {
		return bytefmt.ByteSize(b)
	},
	"ts": func(s string) string {
		return strings.TrimSpace(s)
	},
	"float32f": func(f float32) string {
		return fmt.Sprintf("%.3f", f)
	},
	"divU64": func(a, b uint64) float32 {
		if b == 0 {
			if a > 0 {
				return 100
			}
			return 0
		}
		if a == 0 {
			// 这是从未在线的情况
			return 0.00001 / float32(b) * 100
		}
		return float32(a) / float32(b) * 100
	},
	"div": func(a, b int) float32 {
		if b == 0 {
			if a > 0 {
				return 100
			}
			return 0
		}
		if a == 0 {
			// 这是从未在线的情况
			return 0.00001 / float32(b) * 100
		}
		return float32(a) / float32(b) * 100
	},
	"addU64": func(a, b uint64) uint64 {
		return a + b
	},
	"add": func(a, b int) int {
		return a + b
	},
	"TransLeftPercent": func(a, b float64) (n float64) {
		n, _ = strconv.ParseFloat(fmt.Sprintf("%.2f", (100-(a/b)*100)), 64)
		if n < 0 {
			n = 0
		}
		return
	},
	"TransLeft": func(a, b uint64) string {
		if a < b {
			return "0B"
		}
		return bytefmt.ByteSize(a - b)
	},
	"TransClassName": func(a float64) string {
		if a == 0 {
			return "offline"
		}
		if a > 50 {
			return "fine"
		}
		if a > 20 {
			return "warning"
		}
		if a > 0 {
			return "error"
		}
		return "offline"
	},
	"UintToFloat": func(a uint64) (n float64) {
		n, _ = strconv.ParseFloat((strconv.FormatUint(a, 10)), 64)
		return
	},
	"dayBefore": func(i int) string {
		year, month, day := time.Now().Date()
		today := time.Date(year, month, day, 0, 0, 0, 0, singleton.Loc)
		return today.AddDate(0, 0, i-29).Format("01/02")
	},
	"className": func(percent float32) string {
		if percent == 0 {
			return ""
		}
		if percent > 95 {
			return "good"
		}
		if percent > 80 {
			return "warning"
		}
		return "danger"
	},
	"statusName": func(val float32) string {
		return singleton.StatusCodeToString(singleton.GetStatusCode(val))
	},
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
