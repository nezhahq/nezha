package singleton

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jinzhu/copier"
	"github.com/nezhahq/nezha/model"
	"github.com/nezhahq/nezha/pkg/utils"
	pb "github.com/nezhahq/nezha/proto"
)

const (
	_CurrentStatusSize = 30 // 统计 15 分钟内的数据为当前状态
)

var ServiceSentinelShared *ServiceSentinel

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
	Up    int     // 今日在线计数
	Down  int     // 今日离线计数
	Delay float32 // 今日平均延迟
}

// NewServiceSentinel 创建服务监控器
func NewServiceSentinel(serviceSentinelDispatchBus chan<- model.Service) {
	ServiceSentinelShared = &ServiceSentinel{
		serviceReportChannel:                    make(chan ReportData, 200),
		serviceStatusToday:                      make(map[uint64]*_TodayStatsOfService),
		serviceCurrentStatusIndex:               make(map[uint64]*indexStore),
		serviceCurrentStatusData:                make(map[uint64][]*pb.TaskResult),
		lastStatus:                              make(map[uint64]int),
		serviceResponseDataStoreCurrentUp:       make(map[uint64]uint64),
		serviceResponseDataStoreCurrentDown:     make(map[uint64]uint64),
		serviceResponseDataStoreCurrentAvgDelay: make(map[uint64]float32),
		serviceResponsePing:                     make(map[uint64]map[uint64]*pingStore),
		Services:                                make(map[uint64]*model.Service),
		tlsCertCache:                            make(map[uint64]string),
		// 30天数据缓存
		monthlyStatus: make(map[uint64]*serviceResponseItem),
		dispatchBus:   serviceSentinelDispatchBus,
	}
	// 加载历史记录
	ServiceSentinelShared.loadServiceHistory()

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)

	var mhs []model.ServiceHistory
	// 加载当日记录
	DB.Where("created_at >= ?", today).Find(&mhs)
	totalDelay := make(map[uint64]float32)
	totalDelayCount := make(map[uint64]float32)
	for i := 0; i < len(mhs); i++ {
		totalDelay[mhs[i].ServiceID] += mhs[i].AvgDelay
		totalDelayCount[mhs[i].ServiceID]++
		ServiceSentinelShared.serviceStatusToday[mhs[i].ServiceID].Up += int(mhs[i].Up)
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].TotalUp += mhs[i].Up
		ServiceSentinelShared.serviceStatusToday[mhs[i].ServiceID].Down += int(mhs[i].Down)
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].TotalDown += mhs[i].Down
	}
	for id, delay := range totalDelay {
		ServiceSentinelShared.serviceStatusToday[id].Delay = delay / float32(totalDelayCount[id])
	}

	// 启动服务监控器
	go ServiceSentinelShared.worker()

	// 每日将游标往后推一天
	_, err := Cron.AddFunc("0 0 0 * * *", ServiceSentinelShared.refreshMonthlyServiceStatus)
	if err != nil {
		panic(err)
	}
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
	dispatchBus chan<- model.Service

	serviceResponseDataStoreLock            sync.RWMutex
	serviceStatusToday                      map[uint64]*_TodayStatsOfService // [service_id] -> _TodayStatsOfService
	serviceCurrentStatusIndex               map[uint64]*indexStore           // [service_id] -> 该监控ID对应的 serviceCurrentStatusData 的最新索引下标
	serviceCurrentStatusData                map[uint64][]*pb.TaskResult      // [service_id] -> []model.ServiceHistory
	serviceResponseDataStoreCurrentUp       map[uint64]uint64                // [service_id] -> 当前服务在线计数
	serviceResponseDataStoreCurrentDown     map[uint64]uint64                // [service_id] -> 当前服务离线计数
	serviceResponseDataStoreCurrentAvgDelay map[uint64]float32               // [service_id] -> 当前服务离线计数
	serviceResponsePing                     map[uint64]map[uint64]*pingStore // [service_id] -> ClientID -> delay
	lastStatus                              map[uint64]int
	tlsCertCache                            map[uint64]string

	ServicesLock    sync.RWMutex
	ServiceListLock sync.RWMutex
	Services        map[uint64]*model.Service
	ServiceList     []*model.Service

	// 30天数据缓存
	monthlyStatusLock sync.Mutex
	monthlyStatus     map[uint64]*serviceResponseItem
}

type indexStore struct {
	index int
	t     time.Time
}

type pingStore struct {
	count int
	ping  float32
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
		for i := 0; i < len(v.Up)-1; i++ {
			if i == 0 {
				// 30 天在线率，减去已经出30天之外的数据
				v.TotalDown -= uint64(v.Down[i])
				v.TotalUp -= uint64(v.Up[i])
			}
			v.Up[i], v.Down[i], v.Delay[i] = v.Up[i+1], v.Down[i+1], v.Delay[i+1]
		}
		v.Up[29] = 0
		v.Down[29] = 0
		v.Delay[29] = 0
		// 清理前一天数据
		ss.serviceResponseDataStoreCurrentUp[k] = 0
		ss.serviceResponseDataStoreCurrentDown[k] = 0
		ss.serviceResponseDataStoreCurrentAvgDelay[k] = 0
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
	ss.ServicesLock.RLock()
	defer ss.ServicesLock.RUnlock()

	ss.ServiceListLock.Lock()
	defer ss.ServiceListLock.Unlock()

	ss.ServiceList = utils.MapValuesToSlice(ss.Services)
	slices.SortFunc(ss.ServiceList, func(a, b *model.Service) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

// loadServiceHistory 加载服务监控器的历史状态信息
func (ss *ServiceSentinel) loadServiceHistory() {
	var services []*model.Service
	err := DB.Find(&services).Error
	if err != nil {
		panic(err)
	}

	for i := 0; i < len(services); i++ {
		task := *services[i]
		// 通过cron定时将服务监控任务传递给任务调度管道
		services[i].CronJobID, err = Cron.AddFunc(task.CronSpec(), func() {
			ss.dispatchBus <- task
		})
		if err != nil {
			panic(err)
		}
		ss.Services[services[i].ID] = services[i]
		ss.serviceCurrentStatusData[services[i].ID] = make([]*pb.TaskResult, _CurrentStatusSize)
		ss.serviceStatusToday[services[i].ID] = &_TodayStatsOfService{}
	}
	ss.ServiceList = services

	year, month, day := time.Now().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, Loc)

	for i := 0; i < len(services); i++ {
		ServiceSentinelShared.monthlyStatus[services[i].ID] = &serviceResponseItem{
			service: services[i],
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Up:    &[30]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Down:  &[30]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			},
		}
	}

	// 加载服务监控历史记录
	var mhs []model.ServiceHistory
	DB.Where("created_at > ? AND created_at < ?", today.AddDate(0, 0, -29), today).Find(&mhs)
	var delayCount = make(map[int]int)
	for i := 0; i < len(mhs); i++ {
		dayIndex := 28 - (int(today.Sub(mhs[i].CreatedAt).Hours()) / 24)
		if dayIndex < 0 {
			continue
		}
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].Delay[dayIndex] = (ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].Delay[dayIndex]*float32(delayCount[dayIndex]) + mhs[i].AvgDelay) / float32(delayCount[dayIndex]+1)
		delayCount[dayIndex]++
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].Up[dayIndex] += int(mhs[i].Up)
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].TotalUp += mhs[i].Up
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].Down[dayIndex] += int(mhs[i].Down)
		ServiceSentinelShared.monthlyStatus[mhs[i].ServiceID].TotalDown += mhs[i].Down
	}
}

func (ss *ServiceSentinel) OnServiceUpdate(m model.Service) error {
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.ServicesLock.Lock()
	defer ss.ServicesLock.Unlock()

	var err error
	// 写入新任务
	m.CronJobID, err = Cron.AddFunc(m.CronSpec(), func() {
		ss.dispatchBus <- m
	})
	if err != nil {
		return err
	}
	if ss.Services[m.ID] != nil {
		// 停掉旧任务
		Cron.Remove(ss.Services[m.ID].CronJobID)
	} else {
		// 新任务初始化数据
		ss.monthlyStatus[m.ID] = &serviceResponseItem{
			service: &m,
			ServiceResponseItem: model.ServiceResponseItem{
				Delay: &[30]float32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Up:    &[30]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
				Down:  &[30]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			},
		}
		ss.serviceCurrentStatusData[m.ID] = make([]*pb.TaskResult, _CurrentStatusSize)
		ss.serviceStatusToday[m.ID] = &_TodayStatsOfService{}
	}
	// 更新这个任务
	ss.Services[m.ID] = &m
	return nil
}

func (ss *ServiceSentinel) OnServiceDelete(ids []uint64) {
	ss.serviceResponseDataStoreLock.Lock()
	defer ss.serviceResponseDataStoreLock.Unlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()
	ss.ServicesLock.Lock()
	defer ss.ServicesLock.Unlock()

	for _, id := range ids {
		delete(ss.serviceCurrentStatusIndex, id)
		delete(ss.serviceCurrentStatusData, id)
		delete(ss.lastStatus, id)
		delete(ss.serviceResponseDataStoreCurrentUp, id)
		delete(ss.serviceResponseDataStoreCurrentDown, id)
		delete(ss.serviceResponseDataStoreCurrentAvgDelay, id)
		delete(ss.tlsCertCache, id)
		delete(ss.serviceStatusToday, id)

		// 停掉定时任务
		Cron.Remove(ss.Services[id].CronJobID)
		delete(ss.Services, id)

		delete(ss.monthlyStatus, id)
	}
}

func (ss *ServiceSentinel) LoadStats() map[uint64]*serviceResponseItem {
	ss.ServicesLock.RLock()
	defer ss.ServicesLock.RUnlock()
	ss.serviceResponseDataStoreLock.RLock()
	defer ss.serviceResponseDataStoreLock.RUnlock()
	ss.monthlyStatusLock.Lock()
	defer ss.monthlyStatusLock.Unlock()

	// 刷新最新一天的数据
	for k := range ss.Services {
		ss.monthlyStatus[k].service = ss.Services[k]
		v := ss.serviceStatusToday[k]

		// 30 天在线率，
		//   |- 减去上次加的旧当天数据，防止出现重复计数
		ss.monthlyStatus[k].TotalUp -= uint64(ss.monthlyStatus[k].Up[29])
		ss.monthlyStatus[k].TotalDown -= uint64(ss.monthlyStatus[k].Down[29])
		//   |- 加上当日数据
		ss.monthlyStatus[k].TotalUp += uint64(v.Up)
		ss.monthlyStatus[k].TotalDown += uint64(v.Down)

		ss.monthlyStatus[k].Up[29] = v.Up
		ss.monthlyStatus[k].Down[29] = v.Down
		ss.monthlyStatus[k].Delay[29] = v.Delay
	}

	// 最后 5 分钟的状态 与 service 对象填充
	for k, v := range ss.serviceResponseDataStoreCurrentDown {
		ss.monthlyStatus[k].CurrentDown = v
	}
	for k, v := range ss.serviceResponseDataStoreCurrentUp {
		ss.monthlyStatus[k].CurrentUp = v
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

// worker 服务监控的实际工作流程
func (ss *ServiceSentinel) worker() {
	// 从服务状态汇报管道获取汇报的服务数据
	for r := range ss.serviceReportChannel {
		if ss.Services[r.Data.GetId()] == nil || ss.Services[r.Data.GetId()].ID == 0 {
			log.Printf("NEZHA>> Incorrect service monitor report %+v", r)
			continue
		}
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
				ts.count = 0
				if err := DB.Create(&model.ServiceHistory{
					ServiceID: mh.GetId(),
					AvgDelay:  ts.ping,
					Data:      mh.Data,
					ServerID:  r.Reporter,
				}).Error; err != nil {
					log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
				}
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
		if ss.serviceCurrentStatusIndex[mh.GetId()] == nil {
			ss.serviceCurrentStatusIndex[mh.GetId()] = &indexStore{
				t:     currentTime,
				index: 0,
			}
		}
		// 写入当前数据
		if ss.serviceCurrentStatusIndex[mh.GetId()].t.Before(currentTime) {
			ss.serviceCurrentStatusIndex[mh.GetId()].t = currentTime.Add(30 * time.Second)
			ss.serviceCurrentStatusData[mh.GetId()][ss.serviceCurrentStatusIndex[mh.GetId()].index] = mh
			ss.serviceCurrentStatusIndex[mh.GetId()].index++
		}

		// 更新当前状态
		ss.serviceResponseDataStoreCurrentUp[mh.GetId()] = 0
		ss.serviceResponseDataStoreCurrentDown[mh.GetId()] = 0
		ss.serviceResponseDataStoreCurrentAvgDelay[mh.GetId()] = 0

		// 永远是最新的 30 个数据的状态 [01:00, 02:00, 03:00] -> [04:00, 02:00, 03: 00]
		for i := 0; i < len(ss.serviceCurrentStatusData[mh.GetId()]); i++ {
			if ss.serviceCurrentStatusData[mh.GetId()][i].GetId() > 0 {
				if ss.serviceCurrentStatusData[mh.GetId()][i].Successful {
					ss.serviceResponseDataStoreCurrentUp[mh.GetId()]++
					ss.serviceResponseDataStoreCurrentAvgDelay[mh.GetId()] = (ss.serviceResponseDataStoreCurrentAvgDelay[mh.GetId()]*float32(ss.serviceResponseDataStoreCurrentUp[mh.GetId()]-1) + ss.serviceCurrentStatusData[mh.GetId()][i].Delay) / float32(ss.serviceResponseDataStoreCurrentUp[mh.GetId()])
				} else {
					ss.serviceResponseDataStoreCurrentDown[mh.GetId()]++
				}
			}
		}

		// 计算在线率，
		var upPercent uint64 = 0
		if ss.serviceResponseDataStoreCurrentDown[mh.GetId()]+ss.serviceResponseDataStoreCurrentUp[mh.GetId()] > 0 {
			upPercent = ss.serviceResponseDataStoreCurrentUp[mh.GetId()] * 100 / (ss.serviceResponseDataStoreCurrentDown[mh.GetId()] + ss.serviceResponseDataStoreCurrentUp[mh.GetId()])
		}
		stateCode := GetStatusCode(upPercent)

		// 数据持久化
		if ss.serviceCurrentStatusIndex[mh.GetId()].index == _CurrentStatusSize {
			ss.serviceCurrentStatusIndex[mh.GetId()] = &indexStore{
				index: 0,
				t:     currentTime,
			}
			if err := DB.Create(&model.ServiceHistory{
				ServiceID: mh.GetId(),
				AvgDelay:  ss.serviceResponseDataStoreCurrentAvgDelay[mh.GetId()],
				Data:      mh.Data,
				Up:        ss.serviceResponseDataStoreCurrentUp[mh.GetId()],
				Down:      ss.serviceResponseDataStoreCurrentDown[mh.GetId()],
			}).Error; err != nil {
				log.Printf("NEZHA>> Failed to save service monitor metrics: %v", err)
			}
		}

		// 延迟报警
		if mh.Delay > 0 {
			ss.ServicesLock.RLock()
			if ss.Services[mh.GetId()].LatencyNotify {
				notificationGroupID := ss.Services[mh.GetId()].NotificationGroupID
				minMuteLabel := NotificationMuteLabel.ServiceLatencyMin(mh.GetId())
				maxMuteLabel := NotificationMuteLabel.ServiceLatencyMax(mh.GetId())
				if mh.Delay > ss.Services[mh.GetId()].MaxLatency {
					// 延迟超过最大值
					ServerLock.RLock()
					reporterServer := ServerList[r.Reporter]
					msg := Localizer.Tf("[Latency] %s %2f > %2f, Reporter: %s", ss.Services[mh.GetId()].Name, mh.Delay, ss.Services[mh.GetId()].MaxLatency, reporterServer.Name)
					go SendNotification(notificationGroupID, msg, minMuteLabel)
					ServerLock.RUnlock()
				} else if mh.Delay < ss.Services[mh.GetId()].MinLatency {
					// 延迟低于最小值
					ServerLock.RLock()
					reporterServer := ServerList[r.Reporter]
					msg := Localizer.Tf("[Latency] %s %2f < %2f, Reporter: %s", ss.Services[mh.GetId()].Name, mh.Delay, ss.Services[mh.GetId()].MinLatency, reporterServer.Name)
					go SendNotification(notificationGroupID, msg, maxMuteLabel)
					ServerLock.RUnlock()
				} else {
					// 正常延迟， 清除静音缓存
					UnMuteNotification(notificationGroupID, minMuteLabel)
					UnMuteNotification(notificationGroupID, maxMuteLabel)
				}
			}
			ss.ServicesLock.RUnlock()
		}

		// 状态变更报警+触发任务执行
		if stateCode == StatusDown || stateCode != ss.lastStatus[mh.GetId()] {
			ss.ServicesLock.Lock()
			lastStatus := ss.lastStatus[mh.GetId()]
			// 存储新的状态值
			ss.lastStatus[mh.GetId()] = stateCode

			// 判断是否需要发送通知
			isNeedSendNotification := ss.Services[mh.GetId()].Notify && (lastStatus != 0 || stateCode == StatusDown)
			if isNeedSendNotification {
				ServerLock.RLock()

				reporterServer := ServerList[r.Reporter]
				notificationGroupID := ss.Services[mh.GetId()].NotificationGroupID
				notificationMsg := Localizer.Tf("[%s] %s Reporter: %s, Error: %s", StatusCodeToString(stateCode), ss.Services[mh.GetId()].Name, reporterServer.Name, mh.Data)
				muteLabel := NotificationMuteLabel.ServiceStateChanged(mh.GetId())

				// 状态变更时，清除静音缓存
				if stateCode != lastStatus {
					UnMuteNotification(notificationGroupID, muteLabel)
				}

				go SendNotification(notificationGroupID, notificationMsg, muteLabel)
				ServerLock.RUnlock()
			}

			// 判断是否需要触发任务
			isNeedTriggerTask := ss.Services[mh.GetId()].EnableTriggerTask && lastStatus != 0
			if isNeedTriggerTask {
				ServerLock.RLock()
				reporterServer := ServerList[r.Reporter]
				ServerLock.RUnlock()

				if stateCode == StatusGood && lastStatus != stateCode {
					// 当前状态正常 前序状态非正常时 触发恢复任务
					go SendTriggerTasks(ss.Services[mh.GetId()].RecoverTriggerTasks, reporterServer.ID)
				} else if lastStatus == StatusGood && lastStatus != stateCode {
					// 前序状态正常 当前状态非正常时 触发失败任务
					go SendTriggerTasks(ss.Services[mh.GetId()].FailTriggerTasks, reporterServer.ID)
				}
			}

			ss.ServicesLock.Unlock()
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
				ss.ServicesLock.RLock()
				if ss.Services[mh.GetId()].Notify {
					muteLabel := NotificationMuteLabel.ServiceTLS(mh.GetId(), "network")
					go SendNotification(ss.Services[mh.GetId()].NotificationGroupID, Localizer.Tf("[TLS] Fetch cert info failed, Reporter: %s, Error: %s", ss.Services[mh.GetId()].Name, errMsg), muteLabel)
				}
				ss.ServicesLock.RUnlock()

			}
		} else {
			// 清除网络错误静音缓存
			UnMuteNotification(ss.Services[mh.GetId()].NotificationGroupID, NotificationMuteLabel.ServiceTLS(mh.GetId(), "network"))

			var newCert = strings.Split(mh.Data, "|")
			if len(newCert) > 1 {
				ss.ServicesLock.Lock()
				enableNotify := ss.Services[mh.GetId()].Notify

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

				notificationGroupID := ss.Services[mh.GetId()].NotificationGroupID
				serviceName := ss.Services[mh.GetId()].Name
				ss.ServicesLock.Unlock()

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
						go SendNotification(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), muteLabel)
					}

					// 证书变更提醒
					if isCertChanged {
						errMsg = Localizer.Tf(
							"TLS certificate changed, old: issuer %s, expires at %s; new: issuer %s, expires at %s",
							oldCert[0], expiresOld.Format("2006-01-02 15:04:05"), newCert[0], expiresNew.Format("2006-01-02 15:04:05"))

						// 证书变更后会自动更新缓存，所以不需要静音
						go SendNotification(notificationGroupID, fmt.Sprintf("[TLS] %s %s", serviceName, errMsg), nil)
					}
				}
			}
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

func GetStatusCode[T float32 | uint64](percent T) int {
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

func StatusCodeToString(statusCode int) string {
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
