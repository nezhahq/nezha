package model

import (
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/nezhahq/nezha/pkg/utils"
)

const (
	RuleCoverAll = iota
	RuleCoverIgnoreAll
)

// MaxAlertRuleDuration is the largest duration that can be converted to int
// on every architecture supported by Go. Alert rules are persisted as uint64,
// but their sampling windows use int indexes; keeping the public bound at
// MaxInt32 prevents a crafted value from wrapping during that conversion.
const MaxAlertRuleDuration uint64 = 1<<31 - 1

// MaxAlertRuleCycleInterval also leaves room for the largest integer
// multiplication performed by the calendar helpers (weeks become 7*interval)
// on 32-bit targets.
const MaxAlertRuleCycleInterval uint64 = (1<<31 - 1) / 7

type NResult struct {
	N uint64
}

type Rule struct {
	// 指标类型，cpu、gpu/gpu_max、memory、swap、disk、net_in_speed、net_out_speed
	// net_all_speed、transfer_in、transfer_out、transfer_all、offline
	// transfer_in_cycle、transfer_out_cycle、transfer_all_cycle
	Type          string          `json:"type"`
	Min           float64         `json:"min,omitempty" validate:"optional"`                                                        // 最小阈值 (百分比、字节 kb ÷ 1024)
	Max           float64         `json:"max,omitempty" validate:"optional"`                                                        // 最大阈值 (百分比、字节 kb ÷ 1024)
	CycleStart    *time.Time      `json:"cycle_start,omitempty" validate:"optional"`                                                // 流量统计的开始时间
	CycleInterval uint64          `json:"cycle_interval,omitempty" validate:"optional"`                                             // 流量统计周期
	CycleUnit     string          `json:"cycle_unit,omitempty" enums:"hour,day,week,month,year" validate:"optional" default:"hour"` // 流量统计周期单位，默认hour,可选(hour, day, week, month, year)
	Duration      uint64          `json:"duration,omitempty" validate:"optional"`                                                   // 持续时间 (秒)
	Cover         uint64          `json:"cover"`                                                                                    // 覆盖范围 RuleCoverAll/IgnoreAll
	Ignore        map[uint64]bool `json:"ignore,omitempty" validate:"optional"`                                                     // 覆盖范围的排除

	// 只作为缓存使用，记录下次该检测的时间
	NextTransferAt  map[uint64]time.Time `json:"-"`
	LastCycleStatus map[uint64]bool      `json:"-"`
}

func percentage(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

// IsSupportedType reports whether the rule type has an evaluator. Keep this
// list in lockstep with Snapshot's switch so untrusted strings cannot enter the
// persisted alert pipeline and silently acquire fallback semantics.
func (u *Rule) IsSupportedType() bool {
	if u == nil {
		return false
	}
	switch u.Type {
	case "cpu", "gpu", "gpu_max", "memory", "swap", "disk",
		"net_in_speed", "net_out_speed", "net_all_speed",
		"transfer_in", "transfer_out", "transfer_all", "offline",
		"transfer_in_cycle", "transfer_out_cycle", "transfer_all_cycle",
		"load1", "load5", "load15", "tcp_conn_count", "udp_conn_count",
		"process_count", "temperature_max":
		return true
	default:
		return false
	}
}

// DurationInt converts the persisted duration without allowing uint64-to-int
// truncation. Callers must handle ok=false as an invalid rule.
func (u *Rule) DurationInt() (duration int, ok bool) {
	if u == nil || u.Duration > MaxAlertRuleDuration {
		return 0, false
	}
	return int(u.Duration), true
}

// HasSafeCycleConfiguration guards every value that the cycle evaluator later
// converts or dereferences. It intentionally does not reject an empty unit,
// which is the documented legacy spelling for hours.
func (u *Rule) HasSafeCycleConfiguration() bool {
	return u != nil && u.IsTransferDurationRule() && u.CycleStart != nil &&
		!u.CycleStart.After(time.Now()) && u.CycleInterval > 0 && u.CycleInterval <= MaxAlertRuleCycleInterval &&
		u.HasSafeThresholds() && u.HasSafeCycleUnit()
}

func (u *Rule) HasSafeCycleUnit() bool {
	if u == nil {
		return false
	}
	switch strings.ToLower(u.CycleUnit) {
	case "", "hour", "day", "week", "month", "year":
		return true
	default:
		return false
	}
}

func (u *Rule) HasSafeThresholds() bool {
	if u == nil || math.IsNaN(u.Min) || math.IsNaN(u.Max) || math.IsInf(u.Min, 0) || math.IsInf(u.Max, 0) || u.Min < 0 || u.Max < 0 {
		return false
	}
	return u.Min == 0 || u.Max == 0 || u.Min <= u.Max
}

// Snapshot is retained for callers which only need a boolean result. The alert
// sentinel uses Evaluate so missing telemetry is not mistaken for a breach.
func (u *Rule) Snapshot(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) bool {
	passed, _ := u.Evaluate(cycleTransferStats, server, db)
	return passed
}

// Evaluate returns known=false when a metric cannot be evaluated from the
// current report. Unknown must neither trigger nor resolve an incident.
func (u *Rule) Evaluate(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) (passed, known bool) {
	if server == nil {
		return true, false
	}
	return u.EvaluateRuntime(cycleTransferStats, server, server.RuntimeSnapshot(), db)
}

// EvaluateRuntime evaluates all conditions against the same immutable Agent
// report. Callers evaluating a compound rule must share this snapshot.
func (u *Rule) EvaluateRuntime(cycleTransferStats *CycleTransferStats, server *Server, runtime RuntimeSnapshot, db *gorm.DB) (passed, known bool) {
	if u == nil || server == nil || !u.IsSupportedType() {
		return true, false
	}
	if u.IsTransferDurationRule() && (!u.HasSafeCycleConfiguration() || cycleTransferStats == nil) {
		return true, false
	}

	// 监控全部但是排除了此服务器
	if u.Cover == RuleCoverAll && u.Ignore[server.ID] {
		return true, true
	}
	// 忽略全部但是指定监控了此服务器
	if u.Cover == RuleCoverIgnoreAll && !u.Ignore[server.ID] {
		return true, true
	}

	evalNow := time.Now()
	var cycleStart, cycleEnd time.Time
	if u.IsTransferDurationRule() {
		cycleStart, cycleEnd = u.transferDurationBounds(evalNow)
	}
	// 循环区间流量检测 · 短期无需重复检测
	if u.IsTransferDurationRule() && u.NextTransferAt[server.ID].After(evalNow) {
		return u.LastCycleStatus[server.ID], true
	}

	var src float64
	if u.IsOfflineRule() {
		return !runtime.LastActive.IsZero() && time.Since(runtime.LastActive) <= 6*time.Second, true
	}
	if runtime.State == nil {
		return true, false
	}
	state := runtime.State

	switch u.Type {
	case "cpu":
		src = float64(state.CPU)
	case "gpu", "gpu_max":
		if len(state.GPU) == 0 {
			return true, false
		}
		src = slices.Max(state.GPU)
	case "memory":
		if runtime.Host == nil {
			return true, false
		}
		if runtime.Host.MemTotal == 0 {
			return true, false
		}
		src = percentage(state.MemUsed, runtime.Host.MemTotal)
	case "swap":
		if runtime.Host == nil {
			return true, false
		}
		if runtime.Host.SwapTotal == 0 {
			return true, false
		}
		src = percentage(state.SwapUsed, runtime.Host.SwapTotal)
	case "disk":
		if runtime.Host == nil {
			return true, false
		}
		if runtime.Host.DiskTotal == 0 {
			return true, false
		}
		src = percentage(state.DiskUsed, runtime.Host.DiskTotal)
	case "net_in_speed":
		src = float64(state.NetInSpeed)
	case "net_out_speed":
		src = float64(state.NetOutSpeed)
	case "net_all_speed":
		src = float64(state.NetInSpeed) + float64(state.NetOutSpeed)
	case "transfer_in":
		src = float64(state.NetInTransfer)
	case "transfer_out":
		src = float64(state.NetOutTransfer)
	case "transfer_all":
		src = float64(state.NetOutTransfer + state.NetInTransfer)
	case "transfer_in_cycle":
		if db == nil {
			return true, false
		}
		src = float64(utils.SubUintChecked(state.NetInTransfer, runtime.PrevTransferInSnapshot))
		if u.CycleInterval != 0 {
			var res NResult
			if err := db.Model(&Transfer{}).Select("SUM(`in`) AS n").Where("datetime(`created_at`) >= datetime(?) AND datetime(`created_at`) < datetime(?) AND datetime(`created_at`) <= datetime(?) AND server_id = ?", cycleStart.UTC(), cycleEnd.UTC(), evalNow.UTC(), server.ID).Scan(&res).Error; err != nil {
				log.Printf("NEZHA>> Alert cycle transfer query failed for rule %s server %d: %v", u.Type, server.ID, err)
				return true, false
			}
			src += float64(res.N)
		}
	case "transfer_out_cycle":
		if db == nil {
			return true, false
		}
		src = float64(utils.SubUintChecked(state.NetOutTransfer, runtime.PrevTransferOutSnapshot))
		if u.CycleInterval != 0 {
			var res NResult
			if err := db.Model(&Transfer{}).Select("SUM(`out`) AS n").Where("datetime(`created_at`) >= datetime(?) AND datetime(`created_at`) < datetime(?) AND datetime(`created_at`) <= datetime(?) AND server_id = ?", cycleStart.UTC(), cycleEnd.UTC(), evalNow.UTC(), server.ID).Scan(&res).Error; err != nil {
				log.Printf("NEZHA>> Alert cycle transfer query failed for rule %s server %d: %v", u.Type, server.ID, err)
				return true, false
			}
			src += float64(res.N)
		}
	case "transfer_all_cycle":
		if db == nil {
			return true, false
		}
		src = float64(utils.SubUintChecked(state.NetOutTransfer, runtime.PrevTransferOutSnapshot) + utils.SubUintChecked(state.NetInTransfer, runtime.PrevTransferInSnapshot))
		if u.CycleInterval != 0 {
			var res NResult
			if err := db.Model(&Transfer{}).Select("SUM(`in`+`out`) AS n").Where("datetime(`created_at`) >= datetime(?) AND datetime(`created_at`) < datetime(?) AND datetime(`created_at`) <= datetime(?) AND server_id = ?", cycleStart.UTC(), cycleEnd.UTC(), evalNow.UTC(), server.ID).Scan(&res).Error; err != nil {
				log.Printf("NEZHA>> Alert cycle transfer query failed for rule %s server %d: %v", u.Type, server.ID, err)
				return true, false
			}
			src += float64(res.N)
		}
	case "load1":
		src = state.Load1
	case "load5":
		src = state.Load5
	case "load15":
		src = state.Load15
	case "tcp_conn_count":
		src = float64(state.TcpConnCount)
	case "udp_conn_count":
		src = float64(state.UdpConnCount)
	case "process_count":
		src = float64(state.ProcessCount)
	case "temperature_max":
		var temp []float64
		for _, tempStat := range state.Temperatures {
			if tempStat.Temperature != 0 {
				temp = append(temp, tempStat.Temperature)
			}
		}
		if len(temp) == 0 {
			return true, false
		}
		src = slices.Max(temp)
	default:
		return true, false
	}

	// 循环区间流量检测 · 更新下次需要检测时间
	if u.IsTransferDurationRule() {
		seconds := float64(180)
		if u.Max > 0 {
			seconds = max(1800*((u.Max-src)/u.Max), 180)
		} else if u.Min > 0 {
			seconds = 1800
		}
		if u.NextTransferAt == nil {
			u.NextTransferAt = make(map[uint64]time.Time)
		}
		if u.LastCycleStatus == nil {
			u.LastCycleStatus = make(map[uint64]bool)
		}
		nextAt := evalNow.Add(time.Second * time.Duration(seconds))
		if nextAt.After(cycleEnd) {
			nextAt = cycleEnd
		}
		u.NextTransferAt[server.ID] = nextAt
		if (u.Max > 0 && src > u.Max) || (u.Min > 0 && src < u.Min) {
			u.LastCycleStatus[server.ID] = false
		} else {
			u.LastCycleStatus[server.ID] = true
		}
		if cycleTransferStats.ServerName[server.ID] != server.Name {
			cycleTransferStats.ServerName[server.ID] = server.Name
		}
		cycleTransferStats.Transfer[server.ID] = uint64(src)
		cycleTransferStats.NextUpdate[server.ID] = u.NextTransferAt[server.ID]
		// 自动更新周期流量展示起止时间
		cycleTransferStats.From = cycleStart
		cycleTransferStats.To = cycleEnd
	}

	if (u.Max > 0 && src > u.Max) || (u.Min > 0 && src < u.Min) {
		return false, true
	}

	return true, true
}

// IsTransferDurationRule 判断该规则是否属于周期流量规则 属于则返回true
func (u *Rule) IsTransferDurationRule() bool {
	if u == nil {
		return false
	}
	switch u.Type {
	case "transfer_in_cycle", "transfer_out_cycle", "transfer_all_cycle":
		return true
	default:
		return false
	}
}

func (u *Rule) IsOfflineRule() bool {
	return u != nil && u.Type == "offline"
}

// transferDurationBounds computes both edges from the same clock reading.
// Separate time.Now calls can otherwise straddle a cycle boundary.
func (u *Rule) transferDurationBounds(now time.Time) (time.Time, time.Time) {
	if !u.HasSafeCycleConfiguration() {
		return time.Time{}, time.Time{}
	}
	start := *u.CycleStart
	var end time.Time
	switch strings.ToLower(u.CycleUnit) {
	case "year":
		end = start.AddDate(int(u.CycleInterval), 0, 0)
		for !now.Before(end) {
			start, end = end, end.AddDate(int(u.CycleInterval), 0, 0)
		}
	case "month":
		end = start.AddDate(0, int(u.CycleInterval), 0)
		for !now.Before(end) {
			start, end = end, end.AddDate(0, int(u.CycleInterval), 0)
		}
	case "week":
		end = start.AddDate(0, 0, 7*int(u.CycleInterval))
		for !now.Before(end) {
			start, end = end, end.AddDate(0, 0, 7*int(u.CycleInterval))
		}
	case "day":
		end = start.AddDate(0, 0, int(u.CycleInterval))
		for !now.Before(end) {
			start, end = end, end.AddDate(0, 0, int(u.CycleInterval))
		}
	default: // empty is the legacy spelling for hour
		interval := 3600 * int64(u.CycleInterval)
		start = time.Unix(u.CycleStart.Unix()+(now.Unix()-u.CycleStart.Unix())/interval*interval, 0)
		end = time.Unix(start.Unix()+interval, 0)
	}
	return start, end
}

func (u *Rule) GetTransferDurationStart() time.Time {
	start, _ := u.transferDurationBounds(time.Now())
	return start
}

func (u *Rule) GetTransferDurationEnd() time.Time {
	_, end := u.transferDurationBounds(time.Now())
	return end
}
