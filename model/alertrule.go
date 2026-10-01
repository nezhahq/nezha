package model

import (
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"
	"gorm.io/gorm"
)

const (
	AlertSampleInterval = 3 * time.Second
	AlertSampleMaxAge   = 6 * time.Second
)

// TimedAlertPoint records one distinct observation. Its timestamp is the
// dashboard receipt time of the Agent report, or the sentinel tick for an
// offline-only rule.
type TimedAlertPoint struct {
	At     time.Time
	Values []bool
}

// CheckTimed evaluates the legacy Duration as Duration * 3 seconds, avoiding
// both repeated stale samples and cadence-dependent early incidents. Unknown
// coverage cannot create an incident or a recovery.
func (r *AlertRule) CheckTimed(points []TimedAlertPoint, now time.Time) (known, passed bool) {
	if r == nil || len(r.Rules) == 0 {
		return false, false
	}
	allFailed := true
	for index, rule := range r.Rules {
		if rule == nil || !rule.IsSupportedType() {
			return false, false
		}
		ruleKnown, rulePassed := rule.checkTimed(points, index, now)
		if !ruleKnown {
			return false, false
		}
		if rulePassed {
			allFailed = false
		}
	}
	return true, !allFailed
}

func (rule *Rule) checkTimed(points []TimedAlertPoint, index int, now time.Time) (known, passed bool) {
	if len(points) == 0 {
		return false, false
	}
	if rule.IsTransferDurationRule() {
		last := points[len(points)-1]
		if index >= len(last.Values) || now.Sub(last.At) > AlertSampleMaxAge {
			return false, false
		}
		return true, last.Values[index]
	}
	window := time.Duration(rule.Duration) * AlertSampleInterval
	if window <= 0 {
		return false, false
	}
	end := now.Add(AlertSampleInterval) // the current observation represents one tick
	start := end.Add(-window)
	if points[0].At.After(start) {
		return false, false // the configured wall-clock window has not elapsed
	}
	var covered, failed time.Duration
	for i, point := range points {
		if index >= len(point.Values) {
			return false, false
		}
		segmentStart := point.At
		if start.After(segmentStart) {
			segmentStart = start
		}
		segmentEnd := point.At.Add(AlertSampleMaxAge)
		if end.Before(segmentEnd) {
			segmentEnd = end
		}
		if i+1 < len(points) {
			if points[i+1].At.Before(segmentEnd) {
				segmentEnd = points[i+1].At
			}
		} else {
			if tickEnd := point.At.Add(AlertSampleInterval); tickEnd.Before(segmentEnd) {
				segmentEnd = tickEnd
			}
		}
		if !segmentEnd.After(segmentStart) {
			continue
		}
		span := segmentEnd.Sub(segmentStart)
		covered += span
		if !point.Values[index] {
			failed += span
		}
	}
	if covered < window-window/10 {
		return false, false
	}
	if rule.IsOfflineRule() {
		if covered-failed >= AlertSampleInterval {
			return true, true
		}
		if failed >= window-AlertSampleInterval && covered >= window-AlertSampleInterval {
			return true, false
		}
		return false, false
	}
	if float64(failed) > float64(window)*0.70 {
		return true, false
	}
	if float64(failed+window-covered) <= float64(window)*0.70 {
		return true, true
	}
	return false, false
}

func (r *AlertRule) TimedRetention() time.Duration {
	longest := AlertSampleMaxAge
	for _, rule := range r.Rules {
		if rule == nil || rule.IsTransferDurationRule() {
			continue
		}
		if duration, ok := rule.DurationInt(); ok {
			longest = max(longest, time.Duration(duration)*AlertSampleInterval+AlertSampleMaxAge)
		}
	}
	return longest
}

const (
	ModeAlwaysTrigger  = 0
	ModeOnetimeTrigger = 1
)

type AlertRule struct {
	Common
	Name                   string   `json:"name"`
	RulesRaw               string   `json:"-"`
	Enable                 *bool    `json:"enable,omitempty"`
	TriggerMode            uint8    `gorm:"default:0" json:"trigger_mode"` // 触发模式: 0-始终触发(默认) 1-单次触发
	NotificationGroupID    uint64   `json:"notification_group_id"`         // 该报警规则所在的通知组
	FailTriggerTasksRaw    string   `gorm:"default:'[]'" json:"-"`
	RecoverTriggerTasksRaw string   `gorm:"default:'[]'" json:"-"`
	Rules                  []*Rule  `gorm:"-" json:"rules"`
	FailTriggerTasks       []uint64 `gorm:"-" json:"fail_trigger_tasks"`    // 失败时执行的触发任务id
	RecoverTriggerTasks    []uint64 `gorm:"-" json:"recover_trigger_tasks"` // 恢复时执行的触发任务id
}

func (r *AlertRule) BeforeSave(tx *gorm.DB) error {
	if data, err := json.Marshal(r.Rules); err != nil {
		return err
	} else {
		r.RulesRaw = string(data)
	}
	if data, err := json.Marshal(r.FailTriggerTasks); err != nil {
		return err
	} else {
		r.FailTriggerTasksRaw = string(data)
	}
	if data, err := json.Marshal(r.RecoverTriggerTasks); err != nil {
		return err
	} else {
		r.RecoverTriggerTasksRaw = string(data)
	}
	return nil
}

func (r *AlertRule) AfterFind(tx *gorm.DB) error {
	var err error
	if err = json.Unmarshal([]byte(r.RulesRaw), &r.Rules); err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(r.FailTriggerTasksRaw), &r.FailTriggerTasks); err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(r.RecoverTriggerTasksRaw), &r.RecoverTriggerTasks); err != nil {
		return err
	}
	return nil
}

func (r *AlertRule) Enabled() bool {
	return r.Enable != nil && *r.Enable
}

// IsSafeToEvaluate validates persisted rule data before it reaches the alert
// goroutine. API validation protects new writes; this second boundary protects
// upgrades from malformed or deliberately poisoned rows already in the DB.
func (r *AlertRule) IsSafeToEvaluate() bool {
	if r == nil || len(r.Rules) == 0 {
		return false
	}
	for _, rule := range r.Rules {
		if rule == nil || !rule.IsSupportedType() {
			return false
		}
		if !rule.HasSafeThresholds() {
			return false
		}
		switch rule.Cover {
		case RuleCoverAll, RuleCoverIgnoreAll:
		default:
			return false
		}
		if rule.IsTransferDurationRule() {
			if !rule.HasSafeCycleConfiguration() {
				return false
			}
			continue
		}
		duration, ok := rule.DurationInt()
		if !ok || duration < 3 {
			return false
		}
	}
	return true
}

// HasPermission extends the default owner/admin check with PAT
// server_ids whitelist enforcement. AlertRule.Snapshot fans out across
// every owner-visible server filtered only by Rule.Ignore semantics
// (RuleCoverAll: deny-list; RuleCoverIgnoreAll: allow-list). A
// server-limited PAT must therefore satisfy the same cover-fanout rule
// the cron / service paths use — otherwise it can create or update a
// rule that monitors servers outside its whitelist (admin owner: any
// server in the system).
//
// Unknown Rule.Cover is fail-closed: Snapshot's switch defaults to
// "monitor everything", so persisting it would defeat the PAT cover
// guard. createAlertRule / updateAlertRule should also reject unknown
// covers at write time; this method is the runtime safety net.
func (r *AlertRule) HasPermission(ctx *gin.Context) bool {
	if !r.Common.HasPermission(ctx) {
		return false
	}
	v, ok := ctx.Get(CtxKeyAPIToken)
	if !ok {
		return true
	}
	tok, _ := v.(APITokenAccessor)
	if tok == nil {
		return true
	}
	if wl, ok := tok.(APITokenWhitelistView); ok && len(wl.ServerIDs()) == 0 {
		return true
	}
	for _, rule := range r.Rules {
		if rule == nil {
			continue
		}
		switch rule.Cover {
		case RuleCoverAll:
			denyIDs := make([]uint64, 0, len(rule.Ignore))
			for id, ignored := range rule.Ignore {
				if ignored {
					denyIDs = append(denyIDs, id)
				}
			}
			if !DenyListSafeForLimitedPAT(tok, r.GetUserID(), denyIDs) {
				return false
			}
		case RuleCoverIgnoreAll:
			for id, monitored := range rule.Ignore {
				if monitored && !tok.CanAccessServer(id) {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// Snapshot 对传入的Server进行该报警规则下所有type的检查 返回每项检查结果
func (r *AlertRule) Snapshot(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) []bool {
	point, _ := r.SnapshotStatus(cycleTransferStats, server, db)
	return point
}

// SnapshotStatus preserves the distinction between a healthy measurement and
// telemetry that is unavailable. A compound alert cannot be decided while any
// of its applicable conditions is unknown.
func (r *AlertRule) SnapshotStatus(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) ([]bool, bool) {
	if server == nil {
		return nil, false
	}
	return r.SnapshotStatusWithRuntime(cycleTransferStats, server, server.RuntimeSnapshot(), db)
}

// SnapshotStatusWithRuntime uses one immutable report for every condition.
func (r *AlertRule) SnapshotStatusWithRuntime(cycleTransferStats *CycleTransferStats, server *Server, runtime RuntimeSnapshot, db *gorm.DB) ([]bool, bool) {
	point := make([]bool, len(r.Rules))
	known := true

	for i, rule := range r.Rules {
		if rule == nil || !rule.IsSupportedType() {
			// Invalid persisted rules are ignored instead of being interpreted as
			// a failed condition or allowed to panic the sentinel.
			point[i] = true
			known = false
			continue
		}
		var valid bool
		point[i], valid = rule.EvaluateRuntime(cycleTransferStats, server, runtime, db)
		known = known && valid
	}
	return point, known
}

// Check 传入包含当前报警规则下所有type检查结果 返回报警持续时间与是否通过报警检查(通过则返回true)
func (r *AlertRule) Check(points [][]bool) (int, bool) {
	var hasPassedRule bool
	durations := make([]int, len(r.Rules))
	validRules := 0

	for ruleIndex, rule := range r.Rules {
		if rule == nil || !rule.IsSupportedType() {
			continue
		}
		if rule.IsTransferDurationRule() {
			validRules++
			// 循环区间流量报警
			if durations[ruleIndex] < 1 {
				durations[ruleIndex] = 1
			}
			if hasPassedRule {
				continue
			}
			// 只要最后一次检查超出了规则范围 就认为检查未通过
			if len(points) > 0 {
				lastPoint := points[len(points)-1]
				if ruleIndex >= len(lastPoint) || lastPoint[ruleIndex] {
					hasPassedRule = true
				}
			}
			continue
		}

		duration, ok := rule.DurationInt()
		if !ok || duration <= 0 {
			continue
		}
		validRules++
		if rule.IsOfflineRule() {
			// 离线报警，检查直到最后一次在线的离线采样点是否大于 duration
			if hasPassedRule = boundCheck(len(points), duration, hasPassedRule); hasPassedRule {
				continue
			}
			var fail int
			for _, point := range slices.Backward(points[len(points)-duration:]) {
				fail++
				if ruleIndex >= len(point) || point[ruleIndex] {
					hasPassedRule = true
					break
				}
			}
			durations[ruleIndex] = fail
			continue
		} else {
			// 常规报警
			// duration<=0 是无意义的规则（持续 0 秒）：直接跳过该规则，
			// 既不污染 hasPassedRule（否则会连带跳过同一 alert 里其它有效
			// 规则），也避免下方百分比计算在 total=0 时除零。
			if hasPassedRule = boundCheck(len(points), duration, hasPassedRule); hasPassedRule {
				continue
			}
			if duration > durations[ruleIndex] {
				durations[ruleIndex] = duration
			}
			total, fail := duration, 0
			for timeTick := len(points) - duration; timeTick < len(points); timeTick++ {
				if ruleIndex >= len(points[timeTick]) || !points[timeTick][ruleIndex] {
					fail++
				}
			}
			// 当70%以上的采样点未通过规则判断时 才认为当前检查未通过
			if float64(fail)*100/float64(total) <= 70 {
				hasPassedRule = true
			}
		}
	}

	if validRules == 0 {
		return 0, true
	}
	// 仅当所有检查均未通过时 才触发告警
	return slices.Max(durations), hasPassedRule
}

// RetentionWindow 返回保留历史采样所需的长度（各规则窗口的最大值），只依赖
// 规则定义而非 Check 的判定结果——否则窗口未填满时 Check 返回的 max=0 会被
// 误判为"无需历史"而清空采样，使规则永远攒不够样本。
// 各规则类型回看的采样数必须与 Check 中实际读取的窗口一致：
//   - 周期流量规则：Check 只读最后 1 个采样点 → 需要 1
//   - 离线规则、常规规则：Check 读取 points[len-Duration:] → 需要 Duration
func (r *AlertRule) RetentionWindow() int {
	window := 0
	for _, rule := range r.Rules {
		if rule == nil || !rule.IsSupportedType() {
			continue
		}
		var need int
		if rule.IsTransferDurationRule() {
			need = 1
		} else if d, ok := rule.DurationInt(); ok && d > 0 {
			need = d
		}
		if need > window {
			window = need
		}
	}
	return window
}

func boundCheck(length, duration int, passed bool) bool {
	if passed {
		return true
	}
	// 如果采样点数量不足 则认为检查通过
	return length < duration
}
