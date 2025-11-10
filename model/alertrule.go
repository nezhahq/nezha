package model

import (
	"slices"

	"github.com/goccy/go-json"
	"gorm.io/gorm"
)

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

// Snapshot 对传入的Server进行该报警规则下所有type的检查 返回每项检查结果
func (r *AlertRule) Snapshot(cycleTransferStats *CycleTransferStats, server *Server, db *gorm.DB) []bool {
	point := make([]bool, len(r.Rules))

	for i, rule := range r.Rules {
		point[i] = rule.Snapshot(cycleTransferStats, server, db)
	}
	return point
}

// Check 传入包含当前报警规则下所有type检查结果 返回报警持续时间与是否通过报警检查(通过则返回true)
func (r *AlertRule) Check(points [][]bool) (int, bool) {
	var hasPassedRule bool
	durations := make([]int, len(r.Rules))

	for ruleIndex, rule := range r.Rules {
		duration := int(rule.Duration)
		if rule.IsTransferDurationRule() {
			// 循环区间流量报警
			if durations[ruleIndex] < 1 {
				durations[ruleIndex] = 1
			}
			if hasPassedRule {
				continue
			}
			// 只要最后一次检查超出了规则范围 就认为检查未通过
			if len(points) > 0 && points[len(points)-1][ruleIndex] {
				hasPassedRule = true
			}
		} else if rule.IsOfflineRule() {
			// 离线报警，检查直到最后一次在线的离线采样点是否大于 duration
			if hasPassedRule = boundCheck(len(points), duration, hasPassedRule); hasPassedRule {
				continue
			}
			var fail int
			for _, point := range slices.Backward(points[len(points)-duration:]) {
				fail++
				if point[ruleIndex] {
					hasPassedRule = true
					break
				}
			}
			durations[ruleIndex] = fail
			continue
		} else {
			// 常规报警
			if hasPassedRule = boundCheck(len(points), duration, hasPassedRule); hasPassedRule {
				continue
			}
			if duration > durations[ruleIndex] {
				durations[ruleIndex] = duration
			}
			total, fail := duration, 0
			for timeTick := len(points) - duration; timeTick < len(points); timeTick++ {
				if !points[timeTick][ruleIndex] {
					fail++
				}
			}
			// 当70%以上的采样点未通过规则判断时 才认为当前检查未通过
			if fail*100/total <= 70 {
				hasPassedRule = true
			}
		}
	}

	// 仅当所有检查均未通过时 才触发告警
	return slices.Max(durations), hasPassedRule
}

func boundCheck(length, duration int, passed bool) bool {
	if passed {
		return true
	}
	// 如果采样点数量不足 则认为检查通过
	return length < duration
}
