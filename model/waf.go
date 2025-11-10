package model

import (
	"errors"
	"math/big"
	"time"

	"github.com/nezhahq/nezha/pkg/utils"
	"gorm.io/gorm"
)

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

type WAFApiMock struct {
	IP              string `json:"ip,omitempty"`
	BlockIdentifier int64  `json:"block_identifier,omitempty"`
	BlockReason     uint8  `json:"block_reason,omitempty"`
	BlockTimestamp  uint64 `json:"block_timestamp,omitempty"`
	Count           uint64 `json:"count,omitempty"`
}

type WAF struct {
	IP              []byte `gorm:"type:binary(16);primaryKey" json:"ip,omitempty"`
	BlockIdentifier int64  `gorm:"primaryKey" json:"block_identifier,omitempty"`
	BlockReason     uint8  `json:"block_reason,omitempty"`
	BlockTimestamp  uint64 `gorm:"index" json:"block_timestamp,omitempty"`
	Count           uint64 `json:"count,omitempty"`
}

func (w *WAF) TableName() string {
	return "nz_waf"
}

func CheckIP(db *gorm.DB, ip string) error {
	if ip == "" {
		return nil
	}
	ipBinary, err := utils.IPStringToBinary(ip)
	if err != nil {
		return err
	}

	var blockTimestamp uint64
	result := db.Model(&WAF{}).Order("block_timestamp desc").Select("block_timestamp").Where("ip = ?", ipBinary).Limit(1).Find(&blockTimestamp)
	if result.Error != nil {
		return result.Error
	}

	// 检查是否未找到记录
	if result.RowsAffected < 1 {
		return nil
	}

	var count uint64
	if err := db.Model(&WAF{}).Select("SUM(count)").Where("ip = ?", ipBinary).Scan(&count).Error; err != nil {
		return err
	}

	now := time.Now().Unix()
	if powAdd(count, 4, blockTimestamp) > uint64(now) {
		return errors.New("you were blocked by nezha WAF")
	}
	return nil
}

func UnblockIP(db *gorm.DB, ip string, uid int64) error {
	if ip == "" {
		return nil
	}
	ipBinary, err := utils.IPStringToBinary(ip)
	if err != nil {
		return err
	}
	return db.Unscoped().Delete(&WAF{}, "ip = ? and block_identifier = ?", ipBinary, uid).Error
}

func BatchUnblockIP(db *gorm.DB, ip []string) error {
	if len(ip) < 1 {
		return nil
	}
	ips := make([][]byte, 0, len(ip))
	for _, s := range ip {
		ipBinary, err := utils.IPStringToBinary(s)
		if err != nil {
			continue
		}
		ips = append(ips, ipBinary)
	}
	return db.Unscoped().Delete(&WAF{}, "ip in (?)", ips).Error
}

func BlockIP(db *gorm.DB, ip string, reason uint8, uid int64) error {
	if ip == "" {
		return nil
	}
	ipBinary, err := utils.IPStringToBinary(ip)
	if err != nil {
		return err
	}
	w := WAF{
		IP:              ipBinary,
		BlockIdentifier: uid,
	}
	now := uint64(time.Now().Unix())

	var count any
	if reason == WAFBlockReasonTypeManual {
		count = 99999
	} else {
		count = gorm.Expr("count + 1")
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(&w).Attrs(WAF{
			BlockReason:    reason,
			BlockTimestamp: now,
		}).FirstOrCreate(&w).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE nz_waf SET count = ?, block_reason = ?, block_timestamp = ? WHERE ip = ? and block_identifier = ?", count, reason, now, ipBinary, uid).Error
	})
}

func powAdd(x, y, z uint64) uint64 {
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
