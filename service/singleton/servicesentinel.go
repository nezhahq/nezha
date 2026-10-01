package singleton

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"iter"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"golang.org/x/exp/constraints"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/tsdb"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

const (
	_CurrentStatusSize = 30 // initial capacity for the rolling service window
)

var serviceEventSequence atomic.Uint64

type serviceResponseItem struct {
	model.ServiceResponseItem

	service *model.Service
}

type ReportData struct {
	Data     *pb.TaskResult
	Reporter uint64
}

// _TodayStatsOfService 今日监控记录
type _TodayStatsOfService struct {
	Up    uint64  // 今日在线计数
	Down  uint64  // 今日离线计数
	Delay float64 // 今日平均延迟
}

type serviceResponseData = _TodayStatsOfService

type serviceTaskStatus struct {
	lastStatus    uint8
	stateEpoch    uint64
	lastHistoryAt time.Time
	history       serviceResponseData
	lastData      string
	result        []serviceWindowBucket
}

type serviceProbeSample struct {
	successful bool
	failed     bool
	delay      float64
}

type serviceWindowBucket struct {
	at        time.Time
	reporters map[uint64]serviceProbeSample
}

func (status *serviceTaskStatus) appendResult(now time.Time, reporter uint64, result *pb.TaskResult) {
	bucketAt := now.Truncate(30 * time.Second)
	if len(status.result) > 0 && bucketAt.Before(status.result[len(status.result)-1].at) {
		status.result = nil // wall-clock moved backwards; old buckets are not comparable
	}
	if len(status.result) == 0 || !status.result[len(status.result)-1].at.Equal(bucketAt) {
		status.result = append(status.result, serviceWindowBucket{at: bucketAt, reporters: make(map[uint64]serviceProbeSample)})
	}
	bucket := &status.result[len(status.result)-1]
	sample := bucket.reporters[reporter]
	if result.Successful {
		sample.successful = true
		sample.delay = float64(result.Delay)
	} else {
		sample.failed = true
	}
	bucket.reporters[reporter] = sample
	cutoff := now.Add(-15 * time.Minute)
	first := 0
	for first < len(status.result) && status.result[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		status.result = append([]serviceWindowBucket(nil), status.result[first:]...)
	}
	if result.Successful {
		status.history.Up++
		status.history.Delay = (status.history.Delay*float64(status.history.Up-1) + float64(result.Delay)) / float64(status.history.Up)
	} else {
		status.history.Down++
	}
	status.lastData = result.Data
}

// flushServiceHistory persists the previous non-overlapping reporting period
// before the next observation is added, so long probe gaps cannot lose or
// double-count samples in the 15-minute availability history.
func (ss *ServiceSentinel) flushServiceHistory(serviceID uint64, status *serviceTaskStatus, now time.Time) {
	if status.lastHistoryAt.IsZero() || now.Before(status.lastHistoryAt.Add(15*time.Minute)) {
		return
	}
	if status.history.Up+status.history.Down == 0 {
		status.lastHistoryAt = now
		return
	}
	if !TSDBEnabled() {
		if err := DB.Create(&model.ServiceHistory{
			CreatedAt: status.lastHistoryAt,
			ServiceID: serviceID,
			AvgDelay:  status.history.Delay,
			Data:      status.lastData,
			Up:        status.history.Up,
			Down:      status.history.Down,
		}).Error; err != nil {
			log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
			return
		}
	}
	status.history = serviceResponseData{}
	status.lastHistoryAt = now
}

func (status *serviceTaskStatus) responseData() serviceResponseData {
	var data serviceResponseData
	for _, bucket := range status.result {
		for _, sample := range bucket.reporters {
			if sample.successful {
				data.Up++
				data.Delay = (data.Delay*float64(data.Up-1) + sample.delay) / float64(data.Up)
			}
			if sample.failed {
				data.Down++
			}
		}
	}
	return data
}

type pingStore struct {
	count        int
	ping         float64
	successCount int
}

/*
使用缓存 channel，处理上报的 Service 请求结果，然后判断是否需要报警
需要记录上一次的状态信息

加锁顺序：serviceResponseDataStoreLock > monthlyStatusLock > servicesLock
*/
type ServiceSentinel struct {
	// 服务监控任务上报通道
	serviceReportChannel chan ReportData // 服务状态汇报管道
	// 服务监控任务调度通道
	dispatchBus chan<- *model.Service

	serviceResponseDataStoreLock sync.RWMutex
	serviceStatusToday           map[uint64]*_TodayStatsOfService // [service_id] -> _TodayStatsOfService
	serviceCurrentStatusData     map[uint64]*serviceTaskStatus    // 当前任务结果缓存
	serviceResponseDataStore     map[uint64]serviceResponseData   // 当前数据

	serviceResponsePing                   map[uint64]map[uint64]*pingStore // guarded by serviceResponseDataStoreLock; [service_id] -> ClientID -> delay
	tlsCertCache                          map[uint64]string                // guarded by serviceResponseDataStoreLock
	serviceReportValidatedHook            func(uint64)
	loadStatsResponseLockedHook           func()
	serviceReportBeforeTLSSideEffectsHook func(uint64)

	servicesLock    sync.RWMutex
	serviceListLock sync.RWMutex
	services        map[uint64]*model.Service
	serviceList     []*model.Service

	// 30天数据缓存
	monthlyStatusLock sync.Mutex
	monthlyStatus     map[uint64]*serviceResponseItem

	// closeOnce + workerWG together let Close() wait for the worker goroutine
	// to fully exit. Without this, a test that swaps ServiceSentinelShared back
	// to its original value in t.Cleanup races against the still-running
	// worker, which keeps reading globals like Conf/CronShared/NotificationShared.
	// Production never calls Close() — the process exits while the worker is
	// still running and that is fine — but tests must drain the worker before
	// restoring globals.
	closeOnce sync.Once
	workerWG  sync.WaitGroup
}

// NewServiceSentinel 创建服务监控器
func NewServiceSentinel(serviceSentinelDispatchBus chan<- *model.Service) (*ServiceSentinel, error) {
	ss := &ServiceSentinel{
		serviceReportChannel:     make(chan ReportData, 200),
		serviceStatusToday:       make(map[uint64]*_TodayStatsOfService),
		serviceCurrentStatusData: make(map[uint64]*serviceTaskStatus),
		serviceResponseDataStore: make(map[uint64]serviceResponseData),
		serviceResponsePing:      make(map[uint64]map[uint64]*pingStore),
		services:                 make(map[uint64]*model.Service),
		tlsCertCache:             make(map[uint64]string),
		// 30天数据缓存
		monthlyStatus: make(map[uint64]*serviceResponseItem),
		dispatchBus:   serviceSentinelDispatchBus,
	}

	// 加载历史记录
	err := ss.loadServiceHistory()
	if err != nil {
		return nil, err
	}

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)
	ss.loadTodayStats(today)

	// 启动服务监控器
	ss.workerWG.Add(1)
	go func() {
		defer ss.workerWG.Done()
		ss.worker()
	}()

	// 每日将游标往后推一天
	_, err = CronShared.AddFunc("0 0 0 * * *", ss.refreshMonthlyServiceStatus)
	if err != nil {
		return nil, err
	}

	// 每周日凌晨 4:00 执行系统存储维护
	_, err = CronShared.AddFunc("0 0 4 * * 0", PerformMaintenance)
	if err != nil {
		log.Printf("NEZHA>> Warning: failed to schedule maintenance task: %v", err)
	}

	return ss, nil
}

func (ss *ServiceSentinel) refreshMonthlyServiceStatus() {
	// 刷新数据防止无人访问
	ss.LoadStats()
	// 将数据往前刷一天
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	for k, v := range ss.monthlyStatus {
		for i := range len(v.Up) - 1 {
			if i == 0 {
				// 30 天在线率，减去已经出30天之外的数据
				v.TotalDown -= v.Down[i]
				v.TotalUp -= v.Up[i]
			}
			v.Up[i], v.Down[i], v.Delay[i] = v.Up[i+1], v.Down[i+1], v.Delay[i+1]
		}
		v.Up[29] = 0
		v.Down[29] = 0
		v.Delay[29] = 0
		// 清理前一天数据
		ss.serviceResponseDataStore[k] = serviceResponseData{}
		ss.serviceStatusToday[k].Delay = 0
		ss.serviceStatusToday[k].Up = 0
		ss.serviceStatusToday[k].Down = 0
	}
}

// Dispatch 将传入的 ReportData 传给 服务状态汇报管道
func (ss *ServiceSentinel) Dispatch(r ReportData) {
	ss.serviceReportChannel <- r
}

// sortServices 按 DisplayIndex 降序、ID 升序排列服务列表
func sortServices(services []*model.Service) {
	slices.SortFunc(services, func(a, b *model.Service) int {
		if a.DisplayIndex != b.DisplayIndex {
			return cmp.Compare(b.DisplayIndex, a.DisplayIndex)
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

func (ss *ServiceSentinel) UpdateServiceList() {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	ss.serviceListLock.Lock()
	defer ss.serviceListLock.Unlock()

	ss.serviceList = utils.MapValuesToSlice(ss.services)
	sortServices(ss.serviceList)
}

// loadServiceHistory 加载服务监控器的历史状态信息
func (ss *ServiceSentinel) loadServiceHistory() error {
	var services []*model.Service
	err := DB.Find(&services).Error
	if err != nil {
		return err
	}

	validServices := services[:0]
	for _, service := range services {
		if err := model.ValidateServiceMonitorType(uint64(service.Type)); err != nil {
			// Existing databases may contain values written before Service.Type was
			// constrained. Quarantine them in the database for operator review, but
			// never register a cron job that could dispatch a privileged Agent task.
			log.Printf("NEZHA>> quarantining service %d: %v", service.ID, err)
			continue
		}
		task := service
		// 通过cron定时将服务监控任务传递给任务调度管道
		service.CronJobID, err = CronShared.AddFunc(task.CronSpec(), func() {
			ss.dispatchBus <- task
		})
		if err != nil {
			return err
		}
		ss.services[service.ID] = service
		ss.serviceCurrentStatusData[service.ID] = new(serviceTaskStatus)
		ss.serviceCurrentStatusData[service.ID].result = make([]serviceWindowBucket, 0, _CurrentStatusSize)
		ss.serviceStatusToday[service.ID] = &_TodayStatsOfService{}
		validServices = append(validServices, service)
	}
	services = validServices
	ss.serviceList = services
	sortServices(ss.serviceList)

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)

	for _, service := range services {
		ss.monthlyStatus[service.ID] = &serviceResponseItem{
			service: service,
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float64{},
				Up:    &[30]uint64{},
				Down:  &[30]uint64{},
			},
		}
	}

	if TSDBEnabled() {
		ss.loadMonthlyStatusFromTSDB(services, today)
	} else {
		ss.loadMonthlyStatusFromDB(today)
	}

	return nil
}

func (ss *ServiceSentinel) loadMonthlyStatusFromTSDB(services []*model.Service, today time.Time) {
	for _, service := range services {
		dailyStats, err := TSDBShared.QueryServiceDailyStats(service.ID, today, 30)
		if err != nil {
			log.Printf("NEZHA>> Failed to load TSDB history for service %d: %v", service.ID, err)
			continue
		}
		ms := ss.monthlyStatus[service.ID]
		for i := 0; i < 29; i++ {
			ms.Up[i] = dailyStats[i].Up
			ms.TotalUp += dailyStats[i].Up
			ms.Down[i] = dailyStats[i].Down
			ms.TotalDown += dailyStats[i].Down
			ms.Delay[i] = dailyStats[i].Delay
		}
	}
}

func (ss *ServiceSentinel) loadMonthlyStatusFromDB(today time.Time) {
	var mhs []model.ServiceHistory
	DB.Where("created_at > ? AND created_at < ? AND server_id = 0", today.AddDate(0, 0, -29), today).Find(&mhs)
	delayCount := make(map[uint64]map[int]int)
	for _, mh := range mhs {
		dayIndex := 28 - int(today.Sub(mh.CreatedAt).Hours())/24
		if dayIndex < 0 {
			continue
		}
		ms := ss.monthlyStatus[mh.ServiceID]
		if ms == nil {
			continue
		}
		if delayCount[mh.ServiceID] == nil {
			delayCount[mh.ServiceID] = make(map[int]int)
		}
		ms.Delay[dayIndex] = (ms.Delay[dayIndex]*float64(delayCount[mh.ServiceID][dayIndex]) + mh.AvgDelay) / float64(delayCount[mh.ServiceID][dayIndex]+1)
		delayCount[mh.ServiceID][dayIndex]++
		ms.Up[dayIndex] += mh.Up
		ms.TotalUp += mh.Up
		ms.Down[dayIndex] += mh.Down
		ms.TotalDown += mh.Down
	}
}

func (ss *ServiceSentinel) loadTodayStats(today time.Time) {
	if TSDBEnabled() {
		for serviceID, ms := range ss.monthlyStatus {
			result, err := TSDBShared.QueryServiceHistory(serviceID, tsdb.Period1Day)
			if err != nil {
				log.Printf("NEZHA>> Failed to load TSDB today stats for service %d: %v", serviceID, err)
				continue
			}
			var totalUp, totalDown uint64
			var totalDelay float64
			var delayCount int
			for _, serverStats := range result.Servers {
				totalUp += serverStats.Stats.TotalUp
				totalDown += serverStats.Stats.TotalDown
				if serverStats.Stats.AvgDelay > 0 {
					totalDelay += serverStats.Stats.AvgDelay
					delayCount++
				}
			}
			ss.serviceStatusToday[serviceID].Up = totalUp
			ss.serviceStatusToday[serviceID].Down = totalDown
			if delayCount > 0 {
				ss.serviceStatusToday[serviceID].Delay = totalDelay / float64(delayCount)
			}
			ms.TotalUp += totalUp
			ms.TotalDown += totalDown
		}
	} else {
		var mhs []model.ServiceHistory
		DB.Where("created_at >= ? AND server_id = 0", today).Find(&mhs)
		totalDelay := make(map[uint64]float64)
		totalDelayCount := make(map[uint64]int)
		for _, mh := range mhs {
			ss.serviceStatusToday[mh.ServiceID].Up += mh.Up
			ss.monthlyStatus[mh.ServiceID].TotalUp += mh.Up
			ss.serviceStatusToday[mh.ServiceID].Down += mh.Down
			ss.monthlyStatus[mh.ServiceID].TotalDown += mh.Down
			totalDelay[mh.ServiceID] += mh.AvgDelay
			totalDelayCount[mh.ServiceID]++
		}
		for id, delay := range totalDelay {
			ss.serviceStatusToday[id].Delay = delay / float64(totalDelayCount[id])
		}
	}
}

func (ss *ServiceSentinel) Update(m *model.Service) error {
	if m == nil {
		return fmt.Errorf("service is nil")
	}
	if err := model.ValidateServiceMonitorType(uint64(m.Type)); err != nil {
		return err
	}

	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.Lock()
	defer ss.servicesLock.Unlock()

	var err error
	// 写入新任务
	m.CronJobID, err = CronShared.AddFunc(m.CronSpec(), func() {
		ss.dispatchBus <- m
	})
	if err != nil {
		return err
	}
	// A service edit may disable notifications or change their recipients.
	// Cancel any retry worker holding the previous service configuration.
	NotificationShared.CancelServiceState(m.ID)
	if ss.services[m.ID] != nil {
		// 停掉旧任务
		CronShared.Remove(ss.services[m.ID].CronJobID)
	} else {
		// 新任务初始化数据
		ss.monthlyStatus[m.ID] = &serviceResponseItem{
			service: m,
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float64{},
				Up:    &[30]uint64{},
				Down:  &[30]uint64{},
			},
		}
		if ss.serviceCurrentStatusData[m.ID] == nil {
			ss.serviceCurrentStatusData[m.ID] = new(serviceTaskStatus)
		}
		ss.serviceCurrentStatusData[m.ID].result = make([]serviceWindowBucket, 0, _CurrentStatusSize)
		ss.serviceStatusToday[m.ID] = &_TodayStatsOfService{}
	}
	// 更新这个任务
	ss.services[m.ID] = m
	return nil
}

func (ss *ServiceSentinel) Delete(ids []uint64) {
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.Lock()
	defer ss.servicesLock.Unlock()

	for _, id := range ids {
		NotificationShared.CancelServiceState(id)
		delete(ss.serviceCurrentStatusData, id)
		delete(ss.serviceResponseDataStore, id)
		delete(ss.serviceResponsePing, id)
		delete(ss.tlsCertCache, id)
		delete(ss.serviceStatusToday, id)

		// 停掉定时任务
		// GHSA-jx78-55p5-rwv5 (Finding 2): guard against a caller supplying an id
		// that does not exist in the in-memory registry.  CheckPermission returns
		// vacuously true for unknown ids, so the controller layer cannot prevent
		// this.  Without the guard, ss.services[id] is nil and the .CronJobID
		// field access panics, aborting the Delete loop before the remaining valid
		// ids are cleaned from memory — their service records were already deleted
		// from the database, producing zombie services.
		if svc := ss.services[id]; svc != nil {
			CronShared.Remove(svc.CronJobID)
		}
		delete(ss.services, id)

		delete(ss.monthlyStatus, id)
	}
}

func (ss *ServiceSentinel) LoadStats() map[uint64]*serviceResponseItem {
	ss.serviceResponseDataStoreLock.RLock()
	defer ss.serviceResponseDataStoreLock.RUnlock()
	if ss.loadStatsResponseLockedHook != nil {
		ss.loadStatsResponseLockedHook()
	}
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	// 刷新最新一天的数据
	for k := range ss.services {
		ss.monthlyStatus[k].service = ss.services[k]
		v := ss.serviceStatusToday[k]

		// 30 天在线率，
		//   |- 减去上次加的旧当天数据，防止出现重复计数
		ss.monthlyStatus[k].TotalUp -= ss.monthlyStatus[k].Up[29]
		ss.monthlyStatus[k].TotalDown -= ss.monthlyStatus[k].Down[29]
		//   |- 加上当日数据
		ss.monthlyStatus[k].TotalUp += v.Up
		ss.monthlyStatus[k].TotalDown += v.Down

		ss.monthlyStatus[k].Up[29] = v.Up
		ss.monthlyStatus[k].Down[29] = v.Down
		ss.monthlyStatus[k].Delay[29] = v.Delay
	}

	// 最后 5 分钟的状态 与 service 对象填充
	for k, v := range ss.serviceResponseDataStore {
		ss.monthlyStatus[k].CurrentDown = v.Down
		ss.monthlyStatus[k].CurrentUp = v.Up
	}

	return ss.monthlyStatus
}

func (ss *ServiceSentinel) CopyStats() map[uint64]model.ServiceResponseItem {
	var stats map[uint64]*serviceResponseItem
	copier.Copy(&stats, ss.LoadStats())

	sri := make(map[uint64]model.ServiceResponseItem)
	for k, service := range stats {
		service.ServiceName = service.service.Name
		sri[k] = service.ServiceResponseItem
	}

	return sri
}

func (ss *ServiceSentinel) Get(id uint64) (s *model.Service, ok bool) {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	s, ok = ss.services[id]
	return
}

func (ss *ServiceSentinel) GetList() map[uint64]*model.Service {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	return maps.Clone(ss.services)
}

func (ss *ServiceSentinel) GetSortedList() []*model.Service {
	ss.serviceListLock.RLock()
	defer ss.serviceListLock.RUnlock()

	return slices.Clone(ss.serviceList)
}

func (ss *ServiceSentinel) CheckPermission(c *gin.Context, idList iter.Seq[uint64]) bool {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	for id := range idList {
		if s, ok := ss.services[id]; ok {
			if !s.HasPermission(c) {
				return false
			}
		}
	}
	return true
}

func canReportServiceResult(service *model.Service, reporter *model.Server, taskType uint64) bool {
	if service == nil || reporter == nil || uint64(service.Type) != taskType {
		return false
	}
	switch service.Cover {
	case model.ServiceCoverAll:
		if service.SkipServers[reporter.ID] {
			return false
		}
	case model.ServiceCoverIgnoreAll:
		if !service.SkipServers[reporter.ID] {
			return false
		}
	default:
		return false
	}

	return service.UserID == reporter.GetUserID() || userIsAdmin(service.UserID)
}

// Close shuts down the ServiceSentinel worker goroutine and waits for it to
// exit. It is idempotent and safe to call more than once.
//
// Why this exists: the worker reads multiple package-level globals during
// each report (Conf, CronShared via notifyCheck, NotificationShared via
// UnMuteNotification, ServerShared, TSDBShared). A test fixture that swaps
// those globals out in t.Cleanup MUST first call Close() — otherwise the
// cleanup write races the still-running worker's read and `go test -race`
// fires (see security_regression_test.go newServiceMonitorSecurityHarness).
// Production never calls Close because the process exits with the worker
// still running, which is fine.
func (ss *ServiceSentinel) Close() {
	ss.closeOnce.Do(func() {
		close(ss.serviceReportChannel)
		ss.workerWG.Wait()
	})
}

// worker 服务监控的实际工作流程
//
// IMPORTANT: this loop reads several package-level globals (Conf, CronShared,
// NotificationShared, ServerShared, TSDBShared). Any test that replaces those
// globals via t.Cleanup must first call ServiceSentinel.Close() so the worker
// drains and exits before the swap, otherwise the race detector trips. See
// the Close() comment above for the full rationale.
func (ss *ServiceSentinel) worker() {
	// 从服务状态汇报管道获取汇报的服务数据
	for r := range ss.serviceReportChannel {
		serverShared := ServerShared
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("NEZHA>> Service monitor report processing panicked: %v", recovered)
				}
			}()
			ss.processReport(r, serverShared)
		}()
	}
}

func (ss *ServiceSentinel) processReport(r ReportData, serverShared *ServerClass) {
	serverShared.lockLifecycleRead()
	defer serverShared.unlockLifecycleRead()

	cs, _ := ss.Get(r.Data.GetId())
	reporter, _ := serverShared.Get(r.Reporter)
	// 入站结果必须匹配出站任务派发边界，避免 agent 伪造其他服务 ID 写入监控状态。
	if !canReportServiceResult(cs, reporter, r.Data.GetType()) {
		log.Printf("NEZHA>> Incorrect service monitor report %+v", r)
		return
	}
	if ss.serviceReportValidatedHook != nil {
		ss.serviceReportValidatedHook(r.Data.GetId())
	}

	mh := r.Data
	m := serverShared.GetList()
	// Serialize Delete and Update before this accepted report causes any side effect.
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	serviceStatusToday := ss.serviceStatusToday[mh.GetId()]
	serviceCurrentStatusData := ss.serviceCurrentStatusData[mh.GetId()]
	currentService, serviceExists := ss.Get(mh.GetId())
	if serviceStatusToday == nil || serviceCurrentStatusData == nil || !serviceExists ||
		!canReportServiceResult(currentService, reporter, mh.GetType()) {
		return
	}
	cs = currentService

	if mh.Type == model.TaskTypeTCPPing || mh.Type == model.TaskTypeICMPPing {
		// TCP/ICMP Ping 使用平均值计算后再写入
		serviceTcpMap, ok := ss.serviceResponsePing[mh.GetId()]
		if !ok {
			serviceTcpMap = make(map[uint64]*pingStore)
			ss.serviceResponsePing[mh.GetId()] = serviceTcpMap
		}
		ts, ok := serviceTcpMap[r.Reporter]
		if !ok {
			ts = &pingStore{}
		}
		ts.count++
		ts.ping = (ts.ping*float64(ts.count-1) + float64(mh.Delay)) / float64(ts.count)
		if mh.Successful {
			ts.successCount++
		}
		if ts.count == Conf.AvgPingCount {
			if TSDBEnabled() {
				if err := TSDBShared.WriteServiceMetrics(&tsdb.ServiceMetrics{
					ServiceID:  mh.GetId(),
					ServerID:   r.Reporter,
					Timestamp:  time.Now(),
					Delay:      ts.ping,
					Successful: ts.successCount*2 >= ts.count,
				}); err != nil {
					log.Printf("NEZHA>> Failed to save service monitor metrics to TSDB: %v", err)
				}
			} else {
				if err := DB.Create(&model.ServiceHistory{
					ServiceID: mh.GetId(),
					AvgDelay:  ts.ping,
					Data:      mh.Data,
					ServerID:  r.Reporter,
				}).Error; err != nil {
					log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
				}
			}
			ts.count = 0
			ts.ping = 0
			ts.successCount = 0
		}
		serviceTcpMap[r.Reporter] = ts
	} else {
		if TSDBEnabled() {
			if err := TSDBShared.WriteServiceMetrics(&tsdb.ServiceMetrics{
				ServiceID:  mh.GetId(),
				ServerID:   r.Reporter,
				Timestamp:  time.Now(),
				Delay:      float64(mh.Delay),
				Successful: mh.Successful,
			}); err != nil {
				log.Printf("NEZHA>> Failed to save service monitor metrics to TSDB: %v", err)
			}
		}
	}

	// 写入当天状态
	if mh.Successful {
		serviceStatusToday.Delay = (serviceStatusToday.Delay*float64(serviceStatusToday.Up) +
			float64(mh.Delay)) / float64(serviceStatusToday.Up+1)
		serviceStatusToday.Up++
	} else {
		serviceStatusToday.Down++
	}

	currentTime := time.Now()
	ss.flushServiceHistory(mh.GetId(), serviceCurrentStatusData, currentTime)
	// Include every accepted reporter's result. A fixed count of samples is
	// not a 15-minute window when probes have different intervals or reporters.
	serviceCurrentStatusData.appendResult(currentTime, r.Reporter, mh)
	if serviceCurrentStatusData.lastHistoryAt.IsZero() {
		serviceCurrentStatusData.lastHistoryAt = currentTime
	}

	// 更新当前状态
	ss.serviceResponseDataStore[mh.GetId()] = serviceCurrentStatusData.responseData()

	// 计算在线率，
	var stateCode uint8
	{
		upPercent := uint64(0)
		rd := ss.serviceResponseDataStore[mh.GetId()]
		if rd.Down+rd.Up > 0 {
			upPercent = rd.Up * 100 / (rd.Down + rd.Up)
		}
		if rd.Down+rd.Up == 0 {
			stateCode = StatusNoData
		} else {
			stateCode = GetStatusCode(upPercent)
		}
	}

	// 延迟报警
	if mh.Delay > 0 {
		delayCheck(&r, m, cs, mh)
	}

	// 状态变更报警+触发任务执行
	if stateCode == StatusDown || stateCode != serviceCurrentStatusData.lastStatus {
		lastStatus := serviceCurrentStatusData.lastStatus
		// 存储新的状态值
		serviceCurrentStatusData.lastStatus = stateCode
		if stateCode != lastStatus {
			serviceCurrentStatusData.stateEpoch = serviceEventSequence.Add(1)
		}

		notifyCheck(&r, m, cs, mh, lastStatus, stateCode, serviceCurrentStatusData.stateEpoch)
	}

	// TLS 证书报警仅适用于 HTTPS 探针。其它探针的任意响应文本
	// 不得被当作证书资料解析。
	if mh.Type == model.TaskTypeHTTPGet && strings.HasPrefix(strings.ToLower(cs.Target), "https://") {
		if ss.serviceReportBeforeTLSSideEffectsHook != nil {
			ss.serviceReportBeforeTLSSideEffectsHook(mh.GetId())
		}
		var errMsg string
		if strings.HasPrefix(mh.Data, "SSL证书错误：") {
			// i/o timeout、connection timeout、EOF 错误
			if !strings.HasSuffix(mh.Data, "timeout") &&
				!strings.HasSuffix(mh.Data, "EOF") &&
				!strings.HasSuffix(mh.Data, "timed out") {
				errMsg = mh.Data
				if cs.Notify {
					muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), "network")
					NotificationShared.SendNotificationAsync(cs.NotificationGroupID, Localizer.Tf("[TLS] Fetch cert info failed, Reporter: %s, Error: %s", cs.Name, errMsg), muteLabel)
				}
			}
		} else {
			// 清除网络错误静音缓存
			NotificationShared.UnMuteNotification(cs.NotificationGroupID, NotificationMuteLabel.ServiceTLS(mh.GetId(), "network"))

			var newCert = strings.Split(mh.Data, "|")
			if len(newCert) > 1 {
				enableNotify := cs.Notify
				expiresNew, newErr := time.Parse("2006-01-02 15:04:05 -0700 MST", newCert[1])
				if newErr != nil {
					log.Printf("NEZHA>> Ignoring malformed TLS certificate expiry for service %d", mh.GetId())
					return
				}

				// 首次获取证书信息时，缓存证书信息
				if ss.tlsCertCache[mh.GetId()] == "" {
					ss.tlsCertCache[mh.GetId()] = mh.Data
				}

				oldCert := strings.Split(ss.tlsCertCache[mh.GetId()], "|")
				isCertChanged := false
				expiresOld, oldErr := time.Parse("2006-01-02 15:04:05 -0700 MST", oldCert[1])
				if oldErr != nil {
					log.Printf("NEZHA>> Ignoring malformed TLS certificate expiry for service %d", mh.GetId())
					return
				}

				// 只有通知成功后才推进基线；失败时下一次探针结果会重试。
				if oldCert[0] != newCert[0] || !expiresNew.Equal(expiresOld) {
					isCertChanged = true
					if !enableNotify {
						ss.tlsCertCache[mh.GetId()] = mh.Data
					}
				}

				notificationGroupID := cs.NotificationGroupID
				serviceName := cs.Name

				// 需要发送提醒
				if enableNotify {
					// 证书过期提醒
					if expiresNew.Before(time.Now().AddDate(0, 0, 7)) {
						expiresTimeStr := expiresNew.Format("2006-01-02 15:04:05")
						errMsg = Localizer.Tf(
							"The TLS certificate will expire within seven days. Expiration time: %s",
							expiresTimeStr,
						)

						// 静音规则： 服务id+证书过期时间
						// 用于避免多个监测点对相同证书同时报警
						muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), fmt.Sprintf("expire_%s", expiresTimeStr))
						NotificationShared.SendNotificationAsync(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), muteLabel)
					}

					// 证书变更提醒
					if isCertChanged {
						errMsg = Localizer.Tf(
							"TLS certificate changed, old: issuer %s, expires at %s; new: issuer %s, expires at %s",
							oldCert[0], expiresOld.Format("2006-01-02 15:04:05"), newCert[0], expiresNew.Format("2006-01-02 15:04:05"))

						oldRaw, newRaw, serviceID := ss.tlsCertCache[mh.GetId()], mh.Data, mh.GetId()
						muteLabel := NotificationMuteLabel.ServiceTLS(serviceID, fmt.Sprintf("change_%x", sha256.Sum256([]byte(newRaw))))
						NotificationShared.sendNotificationAsync(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), muteLabel, func() {
							ss.serviceResponseDataStoreLock.Lock()
							if ss.serviceCurrentStatusData[serviceID] != nil && ss.tlsCertCache[serviceID] == oldRaw {
								ss.tlsCertCache[serviceID] = newRaw
							}
							ss.serviceResponseDataStoreLock.Unlock()
						})
					}
				}
			}
		}
	}
}

func delayCheck(r *ReportData, m map[uint64]*model.Server, ss *model.Service, mh *pb.TaskResult) {
	if !ss.LatencyNotify {
		return
	}

	// GHSA-jx78-55p5-rwv5 (incomplete fix of GHSA-qjpp-gffx-2wm9): the server
	// map snapshot m is taken outside serviceResponseDataStoreLock and
	// ServerShared has its own independent lock, so a concurrent batch-delete of
	// the reporter's server can remove the entry between the pre-lock validation
	// and this point.  Guard against the nil pointer before using the server.
	reporterServer := m[r.Reporter]
	if reporterServer == nil {
		return
	}

	notificationGroupID := ss.NotificationGroupID
	minMuteLabel := NotificationMuteLabel.ServiceLatencyMin(mh.GetId())
	maxMuteLabel := NotificationMuteLabel.ServiceLatencyMax(mh.GetId())
	if mh.Delay > ss.MaxLatency {
		// 延迟超过最大值
		msg := Localizer.Tf("[Latency] %s %2f > %2f, Reporter: %s", ss.Name, mh.Delay, ss.MaxLatency, reporterServer.Name)
		NotificationShared.SendNotificationAsync(notificationGroupID, msg, minMuteLabel)
	} else if mh.Delay < ss.MinLatency {
		// 延迟低于最小值
		msg := Localizer.Tf("[Latency] %s %2f < %2f, Reporter: %s", ss.Name, mh.Delay, ss.MinLatency, reporterServer.Name)
		NotificationShared.SendNotificationAsync(notificationGroupID, msg, maxMuteLabel)
	} else {
		// 正常延迟， 清除静音缓存
		NotificationShared.UnMuteNotification(notificationGroupID, minMuteLabel)
		NotificationShared.UnMuteNotification(notificationGroupID, maxMuteLabel)
	}
}

func notifyCheck(r *ReportData, m map[uint64]*model.Server,
	ss *model.Service, mh *pb.TaskResult, lastStatus, stateCode uint8, stateEpoch uint64) {
	// GHSA-jx78-55p5-rwv5: guard against concurrent server deletion (same TOCTOU
	// class as the 2026-07-21 fix, a few dozen lines lower in the same worker).
	// ServerShared has its own lock; m is a snapshot taken outside
	// serviceResponseDataStoreLock, so the server may have been removed between
	// the pre-lock validation and here.
	reporterServer := m[r.Reporter]

	// 判断是否需要发送通知
	isNeedSendNotification := ss.Notify && (lastStatus != 0 || stateCode == StatusDown)
	if isNeedSendNotification && reporterServer != nil {
		notificationGroupID := ss.NotificationGroupID
		notificationMsg := Localizer.Tf("[%s] %s Reporter: %s, Error: %s", StatusCodeToString(stateCode), ss.Name, reporterServer.Name, mh.Data)
		muteLabel := fmt.Sprintf("%s:%d:%d", NotificationMuteLabel.ServiceStateChanged(mh.GetId()), stateCode, stateEpoch)

		NotificationShared.SendServiceState(mh.GetId(), notificationGroupID, notificationMsg, muteLabel)
	}

	// 判断是否需要触发任务
	isNeedTriggerTask := ss.EnableTriggerTask && lastStatus != 0
	if isNeedTriggerTask && reporterServer != nil {
		if stateCode == StatusGood && lastStatus != stateCode {
			// 当前状态正常 前序状态非正常时 触发恢复任务
			go CronShared.SendTriggerTasks(ss.RecoverTriggerTasks, reporterServer.ID, ss.UserID)
		} else if lastStatus == StatusGood && lastStatus != stateCode {
			// 前序状态正常 当前状态非正常时 触发失败任务
			go CronShared.SendTriggerTasks(ss.FailTriggerTasks, reporterServer.ID, ss.UserID)
		}
	}
}

const (
	_ = iota
	StatusNoData
	StatusGood
	StatusLowAvailability
	StatusDown
)

func GetStatusCode[T constraints.Float | constraints.Integer](percent T) uint8 {
	if percent > 95 {
		return StatusGood
	}
	if percent > 80 {
		return StatusLowAvailability
	}
	return StatusDown
}

func StatusCodeToString(statusCode uint8) string {
	switch statusCode {
	case StatusNoData:
		return Localizer.T("No Data")
	case StatusGood:
		return Localizer.T("Good")
	case StatusLowAvailability:
		return Localizer.T("Low Availability")
	case StatusDown:
		return Localizer.T("Down")
	default:
		return ""
	}
}
