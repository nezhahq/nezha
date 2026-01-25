package singleton

import (
	"log"

	"github.com/nezhahq/nezha/pkg/tsdb"
)

var (
	// TSDBShared 全局 TSDB 实例，可能为 nil（未启用）
	TSDBShared *tsdb.TSDB
)

// InitTSDB 初始化 TSDB
// 如果配置中未设置 TSDB DataPath，则不启用 TSDB 功能
func InitTSDB() error {
	config := &tsdb.Config{
		RetentionDays:  30,
		MaxDiskUsageGB: 5,
	}

	// 从配置文件加载 TSDB 配置
	if Conf.TSDB.DataPath != "" {
		config.DataPath = Conf.TSDB.DataPath
	}
	if Conf.TSDB.RetentionDays > 0 {
		config.RetentionDays = Conf.TSDB.RetentionDays
	}
	if Conf.TSDB.MaxDiskUsageGB > 0 {
		config.MaxDiskUsageGB = Conf.TSDB.MaxDiskUsageGB
	}

	// 如果未配置 DataPath，则不启用 TSDB
	if !config.Enabled() {
		log.Println("NEZHA>> TSDB is disabled (tsdb.data_path not configured)")
		return nil
	}

	var err error
	TSDBShared, err = tsdb.Open(config)
	if err != nil {
		return err
	}

	tsdb.SetInstance(TSDBShared)
	log.Println("NEZHA>> TSDB initialized successfully")
	return nil
}

// TSDBEnabled 检查 TSDB 是否已启用
func TSDBEnabled() bool {
	return TSDBShared != nil && !TSDBShared.IsClosed()
}

// CloseTSDB 关闭 TSDB
func CloseTSDB() {
	if TSDBShared != nil {
		TSDBShared.Close()
		TSDBShared = nil
	}
}
