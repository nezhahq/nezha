package singleton

import (
	"cmp"
	"fmt"
	"iter"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"golang.org/x/exp/constraints"

	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

const (
	_CurrentStatusSize = 30 // 统计 15 分钟内的数据为当前状态
)

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
	Delay float32 // 今日平均延迟
}

type serviceResponseData = _TodayStatsOfService

type serviceTaskStatus struct {
	lastStatus uint8
	t          time.Time
	result     []*pb.TaskResult
}

type pingStore struct {
	count int
	ping  float32
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

	serviceResponsePing map[uint64]map[uint64]*pingStore // [service_id] -> ClientID -> delay
	tlsCertCache        map[uint64]string

	servicesLock    sync.RWMutex
	serviceListLock sync.RWMutex
	services        map[uint64]*model.Service
	serviceList     []*model.Service

	// 30天数据缓存
	monthlyStatusLock sync.Mutex
	monthlyStatus     map[uint64]*serviceResponseItem
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

	var mhs []model.ServiceHistory
	// 加载当日记录
	DB.Where("created_at >= ? AND server_id = 0", today).Find(&mhs)
	totalDelay := make(map[uint64]float32)
	totalDelayCount := make(map[uint64]float32)
	for _, mh := range mhs {
		totalDelay[mh.ServiceID] += mh.AvgDelay
		totalDelayCount[mh.ServiceID]++
		ss.serviceStatusToday[mh.ServiceID].Up += mh.Up
		ss.monthlyStatus[mh.ServiceID].TotalUp += mh.Up
		ss.serviceStatusToday[mh.ServiceID].Down += mh.Down
		ss.monthlyStatus[mh.ServiceID].TotalDown += mh.Down
	}
	for id, delay := range totalDelay {
		ss.serviceStatusToday[id].Delay = delay / float32(totalDelayCount[id])
	}

	// 启动服务监控器
	go ss.worker()

	// 每日将游标往后推一天
	_, err = CronShared.AddFunc("0 0 0 * * *", ss.refreshMonthlyServiceStatus)
	if err != nil {
		return nil, err
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

func (ss *ServiceSentinel) UpdateServiceList() {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()

	ss.serviceListLock.Lock()
	defer ss.serviceListLock.Unlock()

	ss.serviceList = utils.MapValuesToSlice(ss.services)
	slices.SortFunc(ss.serviceList, func(a, b *model.Service) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

// loadServiceHistory 加载服务监控器的历史状态信息
func (ss *ServiceSentinel) loadServiceHistory() error {
	var services []*model.Service
	err := DB.Find(&services).Error
	if err != nil {
		return err
	}

	for _, service := range services {
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
		ss.serviceCurrentStatusData[service.ID].result = make([]*pb.TaskResult, 0, _CurrentStatusSize)
		ss.serviceStatusToday[service.ID] = &_TodayStatsOfService{}
	}
	ss.serviceList = services

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)

	for _, service := range services {
		ss.monthlyStatus[service.ID] = &serviceResponseItem{
			service: service,
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Up:    &[30]uint64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Down:  &[30]uint64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			},
		}
	}

	// 加载服务监控历史记录
	var mhs []model.ServiceHistory
	DB.Where("created_at > ? AND created_at < ? AND server_id = 0", today.AddDate(0, 0, -29), today).Find(&mhs)
	var delayCount = make(map[int]int)
	for _, mh := range mhs {
		dayIndex := 28 - (int(today.Sub(mh.CreatedAt).Hours()) / 24)
		if dayIndex < 0 {
			continue
		}
		ss.monthlyStatus[mh.ServiceID].Delay[dayIndex] = (ss.monthlyStatus[mh.ServiceID].Delay[dayIndex]*float32(delayCount[dayIndex]) + mh.AvgDelay) / float32(delayCount[dayIndex]+1)
		delayCount[dayIndex]++
		ss.monthlyStatus[mh.ServiceID].Up[dayIndex] += mh.Up
		ss.monthlyStatus[mh.ServiceID].TotalUp += mh.Up
		ss.monthlyStatus[mh.ServiceID].Down[dayIndex] += mh.Down
		ss.monthlyStatus[mh.ServiceID].TotalDown += mh.Down
	}

	return nil
}

func (ss *ServiceSentinel) Update(m *model.Service) error {
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
	if ss.services[m.ID] != nil {
		// 停掉旧任务
		CronShared.Remove(ss.services[m.ID].CronJobID)
	} else {
		// 新任务初始化数据
		ss.monthlyStatus[m.ID] = &serviceResponseItem{
			service: m,
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Up:    &[30]uint64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Down:  &[30]uint64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			},
		}
		if ss.serviceCurrentStatusData[m.ID] == nil {
			ss.serviceCurrentStatusData[m.ID] = new(serviceTaskStatus)
		}
		ss.serviceCurrentStatusData[m.ID].result = make([]*pb.TaskResult, 0, _CurrentStatusSize)
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
		delete(ss.serviceCurrentStatusData, id)
		delete(ss.serviceResponseDataStore, id)
		delete(ss.tlsCertCache, id)
		delete(ss.serviceStatusToday, id)

		// 停掉定时任务
		CronShared.Remove(ss.services[id].CronJobID)
		delete(ss.services, id)

		delete(ss.monthlyStatus, id)
	}
}

func (ss *ServiceSentinel) LoadStats() map[uint64]*serviceResponseItem {
	ss.servicesLock.RLock()
	defer ss.servicesLock.RUnlock()
	ss.serviceResponseDataStoreLock.RLock()
	defer ss.serviceResponseDataStoreLock.RUnlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()

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
		if !service.service.EnableShowInService {
			delete(stats, k)
			continue
		}

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

// worker 服务监控的实际工作流程
func (ss *ServiceSentinel) worker() {
	// 从服务状态汇报管道获取汇报的服务数据
	for r := range ss.serviceReportChannel {
		css, _ := ss.Get(r.Data.GetId())
		if css == nil || css.ID == 0 {
			log.Printf("NEZHA>> Incorrect service monitor report %+v", r)
			continue
		}
		css = nil

		mh := r.Data
		if mh.Type == model.TaskTypeTCPPing || mh.Type == model.TaskTypeICMPPing {
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
			ts.ping = (ts.ping*float32(ts.count-1) + mh.Delay) / float32(ts.count)
			if ts.count == Conf.AvgPingCount {
				if err := DB.Create(&model.ServiceHistory{
					ServiceID: mh.GetId(),
					AvgDelay:  ts.ping,
					Data:      mh.Data,
					ServerID:  r.Reporter,
				}).Error; err != nil {
					log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
				}
				ts.count = 0
				ts.ping = mh.Delay
			}
			serviceTcpMap[r.Reporter] = ts
		}

		ss.serviceResponseDataStoreLock.Lock()
		// 写入当天状态
		if mh.Successful {
			ss.serviceStatusToday[mh.GetId()].Delay = (ss.serviceStatusToday[mh.
				GetId()].Delay*float32(ss.serviceStatusToday[mh.GetId()].Up) +
				mh.Delay) / float32(ss.serviceStatusToday[mh.GetId()].Up+1)
			ss.serviceStatusToday[mh.GetId()].Up++
		} else {
			ss.serviceStatusToday[mh.GetId()].Down++
		}

		currentTime := time.Now()
		if ss.serviceCurrentStatusData[mh.GetId()].t.IsZero() {
			ss.serviceCurrentStatusData[mh.GetId()].t = currentTime
		}

		// 写入当前数据
		if ss.serviceCurrentStatusData[mh.GetId()].t.Before(currentTime) {
			ss.serviceCurrentStatusData[mh.GetId()].t = currentTime.Add(30 * time.Second)
			ss.serviceCurrentStatusData[mh.GetId()].result = append(ss.serviceCurrentStatusData[mh.GetId()].result, mh)
		}

		// 更新当前状态
		ss.serviceResponseDataStore[mh.GetId()] = serviceResponseData{}

		// 永远是最新的 30 个数据的状态 [01:00, 02:00, 03:00] -> [04:00, 02:00, 03: 00]
		for _, cs := range ss.serviceCurrentStatusData[mh.GetId()].result {
			if cs.GetId() > 0 {
				rd := ss.serviceResponseDataStore[mh.GetId()]
				if cs.Successful {
					rd.Up++
					rd.Delay = (rd.Delay*float32(rd.Up-1) + cs.Delay) / float32(rd.Up)
				} else {
					rd.Down++
				}
				ss.serviceResponseDataStore[mh.GetId()] = rd
			}
		}

		// 计算在线率，
		var stateCode uint8
		{
			upPercent := uint64(0)
			rd := ss.serviceResponseDataStore[mh.GetId()]
			if rd.Down+rd.Up > 0 {
				upPercent = rd.Up * 100 / (rd.Down + rd.Up)
			}
			stateCode = GetStatusCode(upPercent)
		}

		// 数据持久化
		if len(ss.serviceCurrentStatusData[mh.GetId()].result) == _CurrentStatusSize {
			ss.serviceCurrentStatusData[mh.GetId()].t = currentTime
			rd := ss.serviceResponseDataStore[mh.GetId()]
			if err := DB.Create(&model.ServiceHistory{
				ServiceID: mh.GetId(),
				AvgDelay:  rd.Delay,
				Data:      mh.Data,
				Up:        rd.Up,
				Down:      rd.Down,
			}).Error; err != nil {
				log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
			}

			ss.serviceCurrentStatusData[mh.GetId()].result = ss.serviceCurrentStatusData[mh.GetId()].result[:0]
		}

		cs, _ := ss.Get(mh.GetId())
		m := ServerShared.GetList()
		// 延迟报警
		if mh.Delay > 0 {
			delayCheck(&r, m, cs, mh)
		}

		// 状态变更报警+触发任务执行
		if stateCode == StatusDown || stateCode != ss.serviceCurrentStatusData[mh.GetId()].lastStatus {
			lastStatus := ss.serviceCurrentStatusData[mh.GetId()].lastStatus
			// 存储新的状态值
			ss.serviceCurrentStatusData[mh.GetId()].lastStatus = stateCode

			notifyCheck(&r, m, cs, mh, lastStatus, stateCode)
		}
		ss.serviceResponseDataStoreLock.Unlock()

		// TLS 证书报警
		var errMsg string
		if strings.HasPrefix(mh.Data, "SSL证书错误：") {
			// i/o timeout、connection timeout、EOF 错误
			if !strings.HasSuffix(mh.Data, "timeout") &&
				!strings.HasSuffix(mh.Data, "EOF") &&
				!strings.HasSuffix(mh.Data, "timed out") {
				errMsg = mh.Data
				if cs.Notify {
					muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), "network")
					go NotificationShared.SendNotification(cs.NotificationGroupID, Localizer.Tf("[TLS] Fetch cert info failed, Reporter: %s, Error: %s", cs.Name, errMsg), muteLabel)
				}
			}
		} else {
			// 清除网络错误静音缓存
			NotificationShared.UnMuteNotification(cs.NotificationGroupID, NotificationMuteLabel.ServiceTLS(mh.GetId(), "network"))

			var newCert = strings.Split(mh.Data, "|")
			if len(newCert) > 1 {
				enableNotify := cs.Notify

				// 首次获取证书信息时，缓存证书信息
				if ss.tlsCertCache[mh.GetId()] == "" {
					ss.tlsCertCache[mh.GetId()] = mh.Data
				}

				oldCert := strings.Split(ss.tlsCertCache[mh.GetId()], "|")
				isCertChanged := false
				expiresOld, _ := time.Parse("2006-01-02 15:04:05 -0700 MST", oldCert[1])
				expiresNew, _ := time.Parse("2006-01-02 15:04:05 -0700 MST", newCert[1])

				// 证书变更时，更新缓存
				if oldCert[0] != newCert[0] && !expiresNew.Equal(expiresOld) {
					isCertChanged = true
					ss.tlsCertCache[mh.GetId()] = mh.Data
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
						go NotificationShared.SendNotification(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), muteLabel)
					}

					// 证书变更提醒
					if isCertChanged {
						errMsg = Localizer.Tf(
							"TLS certificate changed, old: issuer %s, expires at %s; new: issuer %s, expires at %s",
							oldCert[0], expiresOld.Format("2006-01-02 15:04:05"), newCert[0], expiresNew.Format("2006-01-02 15:04:05"))

						// 证书变更后会自动更新缓存，所以不需要静音
						go NotificationShared.SendNotification(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), "")
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

	notificationGroupID := ss.NotificationGroupID
	minMuteLabel := NotificationMuteLabel.ServiceLatencyMin(mh.GetId())
	maxMuteLabel := NotificationMuteLabel.ServiceLatencyMax(mh.GetId())
	if mh.Delay > ss.MaxLatency {
		// 延迟超过最大值
		reporterServer := m[r.Reporter]
		msg := Localizer.Tf("[Latency] %s %2f > %2f, Reporter: %s", ss.Name, mh.Delay, ss.MaxLatency, reporterServer.Name)
		go NotificationShared.SendNotification(notificationGroupID, msg, minMuteLabel)
	} else if mh.Delay < ss.MinLatency {
		// 延迟低于最小值
		reporterServer := m[r.Reporter]
		msg := Localizer.Tf("[Latency] %s %2f < %2f, Reporter: %s", ss.Name, mh.Delay, ss.MinLatency, reporterServer.Name)
		go NotificationShared.SendNotification(notificationGroupID, msg, maxMuteLabel)
	} else {
		// 正常延迟， 清除静音缓存
		NotificationShared.UnMuteNotification(notificationGroupID, minMuteLabel)
		NotificationShared.UnMuteNotification(notificationGroupID, maxMuteLabel)
	}
}

func notifyCheck(r *ReportData, m map[uint64]*model.Server,
	ss *model.Service, mh *pb.TaskResult, lastStatus, stateCode uint8) {
	// 判断是否需要发送通知
	isNeedSendNotification := ss.Notify && (lastStatus != 0 || stateCode == StatusDown)
	if isNeedSendNotification {
		reporterServer := m[r.Reporter]
		notificationGroupID := ss.NotificationGroupID
		notificationMsg := Localizer.Tf("[%s] %s Reporter: %s, Error: %s", StatusCodeToString(stateCode), ss.Name, reporterServer.Name, mh.Data)
		muteLabel := NotificationMuteLabel.ServiceStateChanged(mh.GetId())

		// 状态变更时，清除静音缓存
		if stateCode != lastStatus {
			NotificationShared.UnMuteNotification(notificationGroupID, muteLabel)
		}

		go NotificationShared.SendNotification(notificationGroupID, notificationMsg, muteLabel)
	}

	// 判断是否需要触发任务
	isNeedTriggerTask := ss.EnableTriggerTask && lastStatus != 0
	if isNeedTriggerTask {
		reporterServer := m[r.Reporter]
		if stateCode == StatusGood && lastStatus != stateCode {
			// 当前状态正常 前序状态非正常时 触发恢复任务
			go CronShared.SendTriggerTasks(ss.RecoverTriggerTasks, reporterServer.ID)
		} else if lastStatus == StatusGood && lastStatus != stateCode {
			// 前序状态正常 当前状态非正常时 触发失败任务
			go CronShared.SendTriggerTasks(ss.FailTriggerTasks, reporterServer.ID)
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
	if percent == 0 {
		return StatusNoData
	}
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
