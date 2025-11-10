package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"github.com/ory/graceful"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/cmd/dashboard/controller"
	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	"github.com/nezhahq/nezha/cmd/dashboard/rpc"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type DashboardCliParam struct {
	Version          bool   // 当前版本号
	ConfigFile       string // 配置文件路径
	DatabaseLocation string // Sqlite3 数据库文件路径
}

var (
	dashboardCliParam DashboardCliParam
	//go:embed *-dist
	frontendDist embed.FS
)

func initSystem(bus chan<- *model.Service) error {
	// 初始化管理员账户
	var usersCount int64
	if err := singleton.DB.Model(&model.User{}).Count(&usersCount).Error; err != nil {
		return err
	}
	if usersCount == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		admin := model.User{
			Username: "admin",
			Password: string(hash),
		}
		if err := singleton.DB.Create(&admin).Error; err != nil {
			return err
		}
	}

	// 启动 singleton 包下的所有服务
	if err := singleton.LoadSingleton(bus); err != nil {
		return err
	}

	// 每天的3:30 对 监控记录 和 流量记录 进行清理
	if _, err := singleton.CronShared.AddFunc("0 30 3 * * *", singleton.CleanServiceHistory); err != nil {
		return err
	}

	// 每小时对流量记录进行打点
	if _, err := singleton.CronShared.AddFunc("0 0 * * * *", func() { singleton.RecordTransferHourlyUsage() }); err != nil {
		return err
	}
	return nil
}

// @title           Nezha Monitoring API
// @version         1.0
// @description     Nezha Monitoring API
// @termsOfService  http://nezhahq.github.io

// @contact.name   API Support
// @contact.url    http://nezhahq.github.io
// @contact.email  hi@nai.ba

// @license.name  Apache 2.0
// @license.url   http://www.apache.org/licenses/LICENSE-2.0.html

// @host      localhost:8008
// @BasePath  /api/v1

// @securityDefinitions.apikey  BearerAuth
// @in header
// @name Authorization

// @externalDocs.description  OpenAPI
// @externalDocs.url          https://swagger.io/resources/open-api/
func main() {
	flag.BoolVar(&dashboardCliParam.Version, "v", false, "查看当前版本号")
	flag.StringVar(&dashboardCliParam.ConfigFile, "c", "data/config.yaml", "配置文件路径")
	flag.StringVar(&dashboardCliParam.DatabaseLocation, "db", "data/sqlite.db", "Sqlite3数据库文件路径")
	flag.Parse()

	if dashboardCliParam.Version {
		fmt.Println(singleton.Version)
		os.Exit(0)
	}

	serviceSentinelDispatchBus := make(chan *model.Service) // 用于传递服务监控任务信息的channel
	// 初始化 dao 包
	if err := utils.FirstError(singleton.InitFrontendTemplates,
		func() error { return singleton.InitConfigFromPath(dashboardCliParam.ConfigFile) },
		singleton.InitTimezoneAndCache,
		func() error { return singleton.InitDBFromPath(dashboardCliParam.DatabaseLocation) },
		func() error { return initSystem(serviceSentinelDispatchBus) }); err != nil {
		log.Fatal(err)
	}

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", singleton.Conf.ListenHost, singleton.Conf.ListenPort))
	if err != nil {
		log.Fatal(err)
	}

	singleton.CleanServiceHistory()
	rpc.DispatchKeepalive()
	go rpc.DispatchTask(serviceSentinelDispatchBus)
	go singleton.AlertSentinelStart()

	grpcHandler := rpc.ServeRPC()
	httpHandler := controller.ServeWeb(frontendDist)
	controller.InitUpgrader()

	muxHandler := newHTTPandGRPCMux(httpHandler, grpcHandler)
	muxServerHTTP := &http.Server{
		Handler:           muxHandler,
		ReadHeaderTimeout: time.Second * 5,
	}
	muxServerHTTP.Protocols = new(http.Protocols)
	muxServerHTTP.Protocols.SetHTTP1(true)
	muxServerHTTP.Protocols.SetUnencryptedHTTP2(true)

	var muxServerHTTPS *http.Server
	if singleton.Conf.HTTPS.ListenPort != 0 {
		muxServerHTTPS = &http.Server{
			Addr:              fmt.Sprintf("%s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort),
			Handler:           muxHandler,
			ReadHeaderTimeout: time.Second * 5,
			TLSConfig: &tls.Config{
				InsecureSkipVerify: singleton.Conf.HTTPS.InsecureTLS,
			},
		}
	}

	errChan := make(chan error, 2)
	errHTTPS := errors.New("error from https server")

	if err := graceful.Graceful(func() error {
		log.Printf("NEZHA>> Dashboard::START ON %s:%d", singleton.Conf.ListenHost, singleton.Conf.ListenPort)
		if singleton.Conf.HTTPS.ListenPort != 0 {
			go func() {
				errChan <- muxServerHTTPS.ListenAndServeTLS(singleton.Conf.HTTPS.TLSCertPath, singleton.Conf.HTTPS.TLSKeyPath)
			}()
			log.Printf("NEZHA>> Dashboard::START ON %s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort)
		}
		go func() {
			errChan <- muxServerHTTP.Serve(l)
		}()
		return <-errChan
	}, func(c context.Context) error {
		log.Println("NEZHA>> Graceful::START")
		singleton.RecordTransferHourlyUsage()
		log.Println("NEZHA>> Graceful::END")
		var err error
		if muxServerHTTPS != nil {
			err = muxServerHTTPS.Shutdown(c)
		}
		return errors.Join(muxServerHTTP.Shutdown(c), utils.IfOr(err != nil, utils.NewWrapError(errHTTPS, err), nil))
	}); err != nil {
		log.Printf("NEZHA>> ERROR: %v", err)
		var wrapError *utils.WrapError
		if errors.As(err, &wrapError) {
			log.Printf("NEZHA>> ERROR HTTPS: %v", wrapError.Unwrap())
		}
	}

	close(errChan)
}

func newHTTPandGRPCMux(httpHandler http.Handler, grpcHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		natConfig := singleton.NATShared.GetNATConfigByDomain(r.Host)
		if natConfig != nil {
			if !natConfig.Enabled {
				c, _ := gin.CreateTestContext(w)
				waf.ShowBlockPage(c, fmt.Errorf("nat host %s is disabled", natConfig.Domain))
				return
			}
			rpc.ServeNAT(w, r, natConfig)
			return
		}
		if r.ProtoMajor == 2 && r.Header.Get("Content-Type") == "application/grpc" &&
			strings.HasPrefix(r.URL.Path, "/"+proto.NezhaService_ServiceDesc.ServiceName) {
			grpcHandler.ServeHTTP(w, r)
			return
		}
		httpHandler.ServeHTTP(w, r)
	})
}
