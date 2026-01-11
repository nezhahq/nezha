package singleton

import (
	_ "embed"
	"fmt"
	"iter"
	"log"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/patrickmn/go-cache"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"sigs.k8s.io/yaml"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
)

var Version = "debug"

var (
	Cache             *cache.Cache
	DB                *gorm.DB
	Loc               *time.Location
	FrontendTemplates []model.FrontendTemplate
	DashboardBootTime = uint64(time.Now().Unix())

	ServerShared          *ServerClass
	ServiceSentinelShared *ServiceSentinel
	DDNSShared            *DDNSClass
	NotificationShared    *NotificationClass
	NATShared             *NATClass
	CronShared            *CronClass
)

//go:embed frontend-templates.yaml
var frontendTemplatesYAML []byte

func InitTimezoneAndCache() error {
	var err error
	Loc, err = time.LoadLocation(Conf.Location)
	if err != nil {
		return err
	}

	Cache = cache.New(5*time.Minute, 10*time.Minute)
	return nil
}

// LoadSingleton 加载子服务并执行
func LoadSingleton(bus chan<- *model.Service) (err error) {
	initI18n() // 加载本地化服务
	initUser() // 加载用户ID绑定表
	NATShared = NewNATClass()
	DDNSShared = NewDDNSClass()
	NotificationShared = NewNotificationClass()
	ServerShared = NewServerClass()
	CronShared = NewCronClass()
	// 最后初始化 ServiceSentinel
	ServiceSentinelShared, err = NewServiceSentinel(bus)
	return
}

// InitFrontendTemplates 从内置文件中加载FrontendTemplates
func InitFrontendTemplates() error {
	err := yaml.Unmarshal(frontendTemplatesYAML, &FrontendTemplates)
	if err != nil {
		return err
	}
	return nil
}

// InitDBFromPath 从给出的文件路径中加载数据库
func InitDBFromPath(path string) error {
	var err error
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{
		CreateBatchSize: 200,
	})
	if err != nil {
		return err
	}
	if Conf.Debug {
		DB = DB.Debug()
	}
	err = DB.AutoMigrate(model.Server{}, model.User{}, model.ServerGroup{}, model.NotificationGroup{},
		model.Notification{}, model.AlertRule{}, model.Service{}, model.NotificationGroupNotification{},
		model.Cron{}, model.ServerGroupServer{}, model.WAF{},
		model.NAT{}, model.DDNSProfile{}, model.NotificationGroupNotification{},
		model.Oauth2Bind{})
	if err != nil {
		return err
	}

	// 初始化 TSDB，使用与数据库相同的数据目录
	dataDir := filepath.Dir(path)
	if err := InitTSDB(dataDir); err != nil {
		return fmt.Errorf("failed to initialize TSDB: %w", err)
	}

	return nil
}

// RecordTransferHourlyUsage 对流量记录进行打点（仅写入 TSDB）
func RecordTransferHourlyUsage(servers ...*model.Server) {
	if TSDBShared == nil {
		return
	}

	var slist iter.Seq[*model.Server]
	if len(servers) > 0 {
		slist = slices.Values(servers)
	} else {
		slist = utils.Seq2To1(ServerShared.Range)
	}

	var count int
	for server := range slist {
		inTransfer := utils.SubUintChecked(server.State.NetInTransfer, server.PrevTransferInSnapshot)
		outTransfer := utils.SubUintChecked(server.State.NetOutTransfer, server.PrevTransferOutSnapshot)
		if inTransfer == 0 && outTransfer == 0 {
			continue
		}
		server.PrevTransferInSnapshot = server.State.NetInTransfer
		server.PrevTransferOutSnapshot = server.State.NetOutTransfer

		if err := TSDBShared.WriteTransfer(server.ID, inTransfer, outTransfer); err != nil {
			log.Printf("NEZHA>> Failed to write transfer to TSDB: %v", err)
		}
		count++
	}

	if count > 0 && Conf.Debug {
		log.Printf("NEZHA>> Saved traffic metrics to TSDB. Affected %d server(s)", count)
	}
}

// CleanMonitorHistory 清理过时的监控数据
// TSDB 有自己的数据保留策略，此函数保留用于未来扩展
func CleanMonitorHistory() {
	// TSDB 数据保留由 TSDB 配置控制，无需手动清理
	if Conf.Debug {
		log.Println("NEZHA>> Monitor history cleanup: TSDB handles retention automatically")
	}
}

// IPDesensitize 根据设置选择是否对IP进行打码处理 返回处理后的IP(关闭打码则返回原IP)
func IPDesensitize(ip string) string {
	if Conf.EnablePlainIPInNotification {
		return ip
	}
	return utils.IPDesensitize(ip)
}

type class[K comparable, V model.CommonInterface] struct {
	list   map[K]V
	listMu sync.RWMutex

	sortedList   []V
	sortedListMu sync.RWMutex
}

func (c *class[K, V]) Get(id K) (s V, ok bool) {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	s, ok = c.list[id]
	return
}

func (c *class[K, V]) GetList() map[K]V {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	return maps.Clone(c.list)
}

func (c *class[K, V]) GetSortedList() []V {
	c.sortedListMu.RLock()
	defer c.sortedListMu.RUnlock()

	return slices.Clone(c.sortedList)
}

func (c *class[K, V]) Range(fn func(k K, v V) bool) {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	for k, v := range c.list {
		if !fn(k, v) {
			break
		}
	}
}

func (c *class[K, V]) CheckPermission(ctx *gin.Context, idList iter.Seq[K]) bool {
	c.listMu.RLock()
	defer c.listMu.RUnlock()

	for id := range idList {
		if s, ok := c.list[id]; ok {
			if !s.HasPermission(ctx) {
				return false
			}
		}
	}
	return true
}
