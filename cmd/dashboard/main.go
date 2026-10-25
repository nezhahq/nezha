package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"github.com/ory/graceful"
	"golang.org/x/crypto/bcrypt"

	"github.com/nezhahq/nezha/cmd/dashboard/controller"
	"github.com/nezhahq/nezha/cmd/dashboard/controller/waf"
	"github.com/nezhahq/nezha/cmd/dashboard/rpc"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/idcodec"
	"github.com/nezhahq/nezha/pkg/utils"
	"github.com/nezhahq/nezha/proto"
	"github.com/nezhahq/nezha/service/singleton"
)

type DashboardCliParam struct {
	Version          bool
	ConfigFile       string
	DatabaseLocation string
}

var (
	dashboardCliParam DashboardCliParam
	//go:embed *-dist
	frontendDist embed.FS
)

func initSystem(bus chan<- *model.Service) error {
	var usersCount int64
	if err := singleton.DB.Model(&model.User{}).Count(&usersCount).Error; err != nil {
		return err
	}
	// Backward-compatible bootstrap state: existing installers and recovery
	// procedures expect the first login on an empty database to be admin/admin.
	// This is not a permanent credential or an authentication-bypass fallback;
	// operators must complete initialization and change it before exposing the
	// Dashboard. Replacing it requires a coordinated installer/migration flow so
	// existing unattended installations are not locked out.
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

	if err := singleton.LoadSingleton(bus); err != nil {
		return err
	}

	if _, err := singleton.CronShared.AddFunc("0 30 3 * * *", singleton.CleanMonitorHistory); err != nil {
		return err
	}

	if _, err := singleton.CronShared.AddFunc("0 0 * * * *", func() { singleton.RecordTransferHourlyUsage() }); err != nil {
		return err
	}

	// 在线会话兜底清扫：状态流异常断开（失联）时由这里关闭进行中的会话
	if _, err := singleton.CronShared.AddFunc("@every 30s", singleton.SweepOnlineSessions); err != nil {
		return err
	}

	// 在线会话保留清理：超出后台设置的统计窗口后删除
	if _, err := singleton.CronShared.AddFunc("0 30 4 * * *", singleton.PruneOnlineSessions); err != nil {
		return err
	}

	if err := singleton.StartJWTSessionGC(); err != nil {
		return err
	}
	return nil
}

func initIDCodec() error {
	return idcodec.Init([]byte(singleton.Conf.JWTSecretKey))
}

// @title           Nezha Monitoring API
// @version         1.0
// @description     Nezha Monitoring API
// @termsOfService  http://nezhahq.github.io

// @contact.name   API Support
// @contact.url    http://nezhahq.github.io
// @contact.email  hi@nai.ba

// @license.name  WTFPL
// @license.url   https://github.com/nezhahq/nezha/blob/master/LICENSE

// @host      localhost:8008
// @BasePath  /api/v1

// @securityDefinitions.apikey  BearerAuth
// @in header
// @name Authorization
// @description JWT session token. Browser/UI flow. Format: `Bearer <jwt>` or cookie `nz-jwt`.

// @securityDefinitions.apikey  APITokenAuth
// @in header
// @name Authorization
// @description Personal Access Token (PAT). Programmatic/CI/LLM flow. Format: `Bearer nzp_<secret>`.
// @description Each endpoint enforces a specific scope; see the `controller` package godoc for the authoritative scope table.

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

	serviceSentinelDispatchBus := make(chan *model.Service)
	if err := utils.FirstError(singleton.InitFrontendTemplates,
		func() error { return singleton.InitConfigFromPath(dashboardCliParam.ConfigFile) },
		initIDCodec,
		singleton.InitTimezoneAndCache,
		func() error {
			if singleton.Conf.Memory.GoMemLimitMB > 0 {
				debug.SetMemoryLimit(singleton.Conf.Memory.GoMemLimitMB * 1024 * 1024)
				log.Printf("NEZHA>> Go memory limit set to %d MB", singleton.Conf.Memory.GoMemLimitMB)
			}
			return nil
		},
		func() error { return singleton.InitDBFromPath(dashboardCliParam.DatabaseLocation) },
		singleton.InitTSDB,
		func() error { return initSystem(serviceSentinelDispatchBus) }); err != nil {
		log.Fatal(err)
	}

	l, err := openDashboardListener("tcp", dashboardListenerAddress(singleton.Conf.ListenHost, singleton.Conf.ListenPort), dashboardHTTPListener)
	if err != nil {
		log.Fatal(err)
	}
	receiptListener, err := openReceiptGateListener()
	if err != nil {
		log.Fatal(err)
	}
	if receiptListener != nil {
		defer receiptListener.Close()
		rpc.SetReceiptGateListener(receiptListener)
	}

	singleton.CleanMonitorHistory()
	rpc.DispatchKeepalive()
	rpc.SetMCPKillSwitchObserver(func() bool {
		return singleton.Conf == nil || !singleton.Conf.MCPEnabled()
	})
	go rpc.DispatchTask(serviceSentinelDispatchBus)
	go singleton.AlertSentinelStart()

	grpcHandler := rpc.ServeRPC()
	httpHandler := controller.ServeWeb(frontendDist)
	controller.InitUpgrader()
	wireNATDashboardCredentialGate()

	muxHandler := newHTTPandGRPCMux(httpHandler, grpcHandler)
	muxServerHTTP := newDashboardHTTPServer(muxHandler)

	muxServerHTTP.Protocols = new(http.Protocols)
	muxServerHTTP.Protocols.SetHTTP1(true)
	muxServerHTTP.Protocols.SetUnencryptedHTTP2(true)

	var muxServerHTTPS *http.Server
	if singleton.Conf.HTTPS.ListenPort != 0 {
		muxServerHTTPS = &http.Server{
			ReadHeaderTimeout: dashboardReadHeaderTimeout,
			Addr:              fmt.Sprintf("%s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort),
			Handler:           muxHandler,
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
				errChan <- serveDashboardHTTPS(muxServerHTTPS, singleton.Conf.HTTPS.TLSCertPath, singleton.Conf.HTTPS.TLSKeyPath)
			}()
			log.Printf("NEZHA>> Dashboard::START ON %s:%d", singleton.Conf.ListenHost, singleton.Conf.HTTPS.ListenPort)
		}
		go func() {
			errChan <- muxServerHTTP.Serve(l)
		}()
		return <-errChan
	}, func(c context.Context) error {
		log.Println("NEZHA>> Graceful::START")
		rpc.CloseReceiptGate()
		singleton.RecordTransferHourlyUsage()
		singleton.CloseTSDB()
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

// wireNATDashboardCredentialGate connects the NAT ingress to the real
// dashboard-credential classifiers. The NAT ingress strips dashboard-issued
// credentials (panel JWT / panel PAT) from every channel the panel's own
// TokenLookup accepts — the Authorization header, the ?token= query parameter
// and the nz-jwt cookie — before tunneling a request to the agent, and
// forwards foreign values untouched. ServeWeb built the JWT parser the
// classifier reuses, so the gate is wired right after it and before any
// listener starts serving.
func wireNATDashboardCredentialGate() {
	rpc.SetNATDashboardCredentialGate(controller.ClassifyDashboardCredentialValues)
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
