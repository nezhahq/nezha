package model

import (
	"errors"
	"math/big"
	"time"

	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

var ErrIPBlocked = errors.New("you were blocked by nezha WAF")

const (
	_ uint8 = iota
	WAFBlockReasonTypeLoginFail
	WAFBlockReasonTypeBruteForceToken
	WAFBlockReasonTypeAgentAuthFail
	WAFBlockReasonTypeManual
	WAFBlockReasonTypeBruteForceOauth2
)

const (
	BlockIDgRPC = -127 + iota
	BlockIDToken
	BlockIDUnknownUser
	BlockIDManual
)

// WAF 封禁记录
type WAF struct {
	IP             string `gorm:"primaryKey" json:"ip,omitempty"`
	BlockReason    uint8  `json:"block_reason,omitempty"`
	BlockTimestamp uint64 `gorm:"default:0" json:"block_timestamp,omitempty"`
	Count          uint64 `gorm:"default:0" json:"count,omitempty"`
}

// WAFApiMock API 响应结构
type WAFApiMock struct {
	IP             string    `json:"ip,omitempty"`
	BlockReason    uint8     `json:"block_reason,omitempty"`
	BlockTimestamp time.Time `json:"block_timestamp,omitempty"`
	Count          uint64    `json:"count,omitempty"`
}

// WAFCheckFunc WAF 检查函数类型
type WAFCheckFunc func(ip string) error

// WAFBlockFunc WAF 封禁函数类型
type WAFBlockFunc func(ip string, reason uint8) error

// CheckIP 检查 IP 是否被封禁
func (w *WAF) CheckIP(db *gorm.DB, ip string) error {
	if ip == "" {
		return nil
	}
	if err := db.First(w, "ip = ?", ip).Error; err != nil {
		return nil
	}
	if PowAdd(w.Count, 4, w.BlockTimestamp) > uint64(time.Now().Unix()) {
		return ErrIPBlocked
	}
	return nil
}

// BlockIP 封禁 IP
func (w *WAF) BlockIP(db *gorm.DB, ip string, reason uint8) error {
	if ip == "" {
		return nil
	}
	var blockTimestamp uint64
	if reason == WAFBlockReasonTypeManual {
		w.Count = 99999
		blockTimestamp = uint64(time.Now().Unix())
	} else {
		// 计算封禁间隔
		if PowAdd(w.Count+1, 3, w.BlockTimestamp) < uint64(time.Now().Unix()) {
			w.Count = 0
		}
		w.Count++
		blockTimestamp = uint64(time.Now().Unix())
	}
	w.BlockTimestamp = blockTimestamp
	w.BlockReason = reason
	return db.Save(&WAF{
		IP:             ip,
		BlockTimestamp: w.BlockTimestamp,
		Count:          w.Count,
		BlockReason:    reason,
	}).Error
}

// UnblockIP 解封 IP
func UnblockIP(db *gorm.DB, ip string) error {
	if ip == "" {
		return nil
	}
	return db.Unscoped().Delete(&WAF{}, "ip = ?", ip).Error
}

// BatchUnblockIP 批量解封 IP
func BatchUnblockIP(db *gorm.DB, ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	return db.Unscoped().Delete(&WAF{}, "ip in (?)", ips).Error
}

// PowAdd 计算 x^y + z，用于封禁时间计算
func PowAdd(x, y, z uint64) uint64 {
	base := big.NewInt(0).SetUint64(x)
	exp := big.NewInt(0).SetUint64(y)
	result := big.NewInt(1)
	result.Exp(base, exp, nil)
	result.Add(result, big.NewInt(0).SetUint64(z))
	if !result.IsUint64() {
		return ^uint64(0) // return max uint64 value on overflow
	}
	ret := result.Uint64()
	return utils.IfOr(ret < z+3, z+3, ret)
}
