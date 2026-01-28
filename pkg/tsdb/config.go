package tsdb

import "time"

// Config TSDB 配置选项
type Config struct {
	// DataPath 数据存储路径，为空则不启用 TSDB
	DataPath string `koanf:"data_path" json:"data_path,omitempty"`
	// RetentionDays 数据保留天数，默认 30 天
	RetentionDays uint16 `koanf:"retention_days" json:"retention_days,omitempty"`
	// MinFreeDiskSpaceGB 最小磁盘剩余空间(GB)，默认 1GB
	// 当磁盘剩余空间低于此值时，TSDB 将停止接收新数据以防止磁盘耗尽
	MinFreeDiskSpaceGB float64 `koanf:"min_free_disk_space_gb" json:"min_free_disk_space_gb,omitempty"`
	// MaxMemoryMB 最大内存使用量(MB)，默认 256MB，用于限制 VictoriaMetrics 缓存
	MaxMemoryMB int64 `koanf:"max_memory_mb" json:"max_memory_mb,omitempty"`
	// DedupInterval 去重间隔，默认 30 秒
	DedupInterval time.Duration `koanf:"dedup_interval" json:"dedup_interval,omitempty"`
	// WriteBufferSize 写入缓冲区大小，默认 512，达到此数量后批量写入
	WriteBufferSize int `koanf:"write_buffer_size" json:"write_buffer_size,omitempty"`
	// WriteBufferFlushInterval 写入缓冲区刷新间隔，默认 5 秒
	WriteBufferFlushInterval time.Duration `koanf:"write_buffer_flush_interval" json:"write_buffer_flush_interval,omitempty"`
}

// DefaultConfig 返回默认配置（不设置 DataPath，需要显式配置才启用）
func DefaultConfig() *Config {
	return &Config{
		DataPath:                 "",
		RetentionDays:            30,
		MinFreeDiskSpaceGB:       1,
		MaxMemoryMB:              256,
		DedupInterval:            30 * time.Second,
		WriteBufferSize:          512,
		WriteBufferFlushInterval: 5 * time.Second,
	}
}

// Validate 验证配置有效性并填充默认值
func (c *Config) Validate() {
	if c.RetentionDays == 0 {
		c.RetentionDays = 30
	}
	if c.MinFreeDiskSpaceGB <= 0 {
		c.MinFreeDiskSpaceGB = 1
	}
	if c.MaxMemoryMB <= 0 {
		c.MaxMemoryMB = 256
	}
	if c.DedupInterval <= 0 {
		c.DedupInterval = 30 * time.Second
	}
	if c.WriteBufferSize <= 0 {
		c.WriteBufferSize = 512
	}
	if c.WriteBufferFlushInterval <= 0 {
		c.WriteBufferFlushInterval = 5 * time.Second
	}
}

// Enabled 检查是否启用 TSDB
func (c *Config) Enabled() bool {
	return c.DataPath != ""
}

// MinFreeDiskSpaceBytes 返回最小磁盘剩余空间（字节）
func (c *Config) MinFreeDiskSpaceBytes() int64 {
	return int64(c.MinFreeDiskSpaceGB * 1024 * 1024 * 1024)
}
