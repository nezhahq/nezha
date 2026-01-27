package tsdb

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
)

// TSDB 封装 VictoriaMetrics 存储
type TSDB struct {
	storage *storage.Storage
	config  *Config
	mu      sync.RWMutex
	closed  bool

	writer *bufferedWriter
}

var instance *TSDB

// Open 打开或创建 TSDB 存储
func Open(config *Config) (*TSDB, error) {
	if config == nil {
		config = DefaultConfig()
	}

	config.Validate()

	dataPath := config.DataPath
	if !filepath.IsAbs(dataPath) {
		absPath, err := filepath.Abs(dataPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path: %w", err)
		}
		dataPath = absPath
	}

	memBytes := int(config.MaxMemoryMB * 1024 * 1024)
	storage.SetTSIDCacheSize(memBytes * 37 / 100)
	storage.SetMetricNameCacheSize(memBytes * 10 / 100)
	storage.SetMetadataStorageSize(memBytes * 1 / 100)

	storage.SetDedupInterval(config.DedupInterval)
	storage.SetFreeDiskSpaceLimit(config.FreeDiskSpaceLimitBytes())
	storage.SetDataFlushInterval(5 * time.Second)

	opts := storage.OpenOptions{
		Retention: time.Duration(config.RetentionDays) * 24 * time.Hour,
	}

	stor := storage.MustOpenStorage(dataPath, opts)

	db := &TSDB{
		storage: stor,
		config:  config,
	}

	db.writer = newBufferedWriter(db, config.WriteBufferSize, config.WriteBufferFlushInterval)

	log.Printf("NEZHA>> TSDB opened at %s, retention: %d days, max disk: %.1f GB, max memory: %d MB",
		dataPath, config.RetentionDays, config.MaxDiskUsageGB, config.MaxMemoryMB)

	return db, nil
}

// GetInstance 获取全局 TSDB 实例
func GetInstance() *TSDB {
	return instance
}

// SetInstance 设置全局 TSDB 实例
func SetInstance(db *TSDB) {
	instance = db
}

// Close 关闭 TSDB 存储
func (db *TSDB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}

	if db.writer != nil {
		db.writer.stop()
	}

	db.storage.MustClose()
	db.closed = true
	log.Println("NEZHA>> TSDB closed")
	return nil
}

// Storage 返回底层存储对象（用于高级查询）
func (db *TSDB) Storage() *storage.Storage {
	return db.storage
}

// Config 返回配置
func (db *TSDB) Config() *Config {
	return db.config
}

// IsClosed 检查是否已关闭
func (db *TSDB) IsClosed() bool {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.closed
}

// Flush 强制刷盘（主要用于测试）
func (db *TSDB) Flush() {
	if db.writer != nil {
		db.writer.flush()
	}
	db.storage.DebugFlush()
}
