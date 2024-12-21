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
)

const (
	BlockIDgRPC = -127 + iota
	BlockIDToken
	BlockIDUnknownUser
)

type WAFApiMock struct {
	ID              uint64 `json:"id,omitempty"`
	IP              string `json:"ip,omitempty"`
	BlockReason     uint8  `json:"block_reason,omitempty"`
	BlockTimestamp  uint64 `json:"block_timestamp,omitempty"`
	BlockIdentifier uint64 `json:"block_identifier,omitempty"`
}

type WAF struct {
	ID              uint64 `gorm:"primaryKey" json:"id,omitempty"`
	IP              []byte `gorm:"type:binary(16);index:idx_block_identifier" json:"ip,omitempty"`
	BlockReason     uint8  `json:"block_reason,omitempty"`
	BlockTimestamp  uint64 `json:"block_timestamp,omitempty"`
	BlockIdentifier int64  `gorm:"index:idx_block_identifier" json:"block_identifier,omitempty"`
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
	result := db.Model(&WAF{}).Select("block_timestamp").Order("id desc").Where("ip = ?", ipBinary).Limit(1).Find(&blockTimestamp)
	if result.Error != nil {
		return result.Error
	}

	// 检查是否未找到记录
	if result.RowsAffected < 1 {
		return nil
	}

	var count int64
	if err := db.Model(&WAF{}).Where("ip = ?", ipBinary).Count(&count).Error; err != nil {
		return err
	}

	now := time.Now().Unix()
	if powAdd(uint64(count), 4, blockTimestamp) > uint64(now) {
		return errors.New("you are blocked by nezha WAF")
	}
	return nil
}

func ClearIP(db *gorm.DB, ip string, uid int64) error {
	if ip == "" {
		return nil
	}
	ipBinary, err := utils.IPStringToBinary(ip)
	if err != nil {
		return err
	}
	return db.Unscoped().Delete(&WAF{}, "ip = ? and block_identifier = ?", ipBinary, uid).Error
}

func BatchClearIP(db *gorm.DB, ip []string) error {
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
		BlockReason:     reason,
		BlockTimestamp:  uint64(time.Now().Unix()),
		BlockIdentifier: uid,
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var lastRecord WAF
		if err := tx.Model(&WAF{}).Order("id desc").Where("ip = ?", ipBinary).First(&lastRecord).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		return tx.Create(&w).Error
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
