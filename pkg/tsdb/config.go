package tsdb

import "time"

// Config TSDB 配置选项
type Config struct {
	// DataPath 数据存储路径，为空则不启用 TSDB
	DataPath string `koanf:"data_path" json:"data_path,omitempty"`
	// RetentionDays 数据保留天数，默认 30 天
	RetentionDays uint16 `koanf:"retention_days" json:"retention_days,omitempty"`
	// MaxDiskUsageGB 最大磁盘使用量(GB)，默认 5GB
	MaxDiskUsageGB float64 `koanf:"max_disk_usage_gb" json:"max_disk_usage_gb,omitempty"`
	// DedupInterval 去重间隔，默认 30 秒
	DedupInterval time.Duration `koanf:"dedup_interval" json:"dedup_interval,omitempty"`
}

// DefaultConfig 返回默认配置（不设置 DataPath，需要显式配置才启用）
func DefaultConfig() *Config {
	return &Config{
		DataPath:       "", // 默认为空，不启用 TSDB
		RetentionDays:  30,
		MaxDiskUsageGB: 5,
		DedupInterval:  30 * time.Second,
	}
}

// Enabled 检查是否启用 TSDB
func (c *Config) Enabled() bool {
	return c.DataPath != ""
}

// MaxDiskUsageBytes 返回最大磁盘使用量（字节）
func (c *Config) MaxDiskUsageBytes() int64 {
	return int64(c.MaxDiskUsageGB * 1024 * 1024 * 1024)
}

// FreeDiskSpaceLimitBytes 返回磁盘空闲空间限制（字节）
// 设置为最大使用量的 10%，确保不会耗尽磁盘
func (c *Config) FreeDiskSpaceLimitBytes() int64 {
	return int64(c.MaxDiskUsageGB * 1024 * 1024 * 1024 * 0.1)
}
