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
}

var (
	instance *TSDB
	once     sync.Once
)

// Open 打开或创建 TSDB 存储
func Open(config *Config) (*TSDB, error) {
	if config == nil {
		config = DefaultConfig()
	}

	// 确保数据路径是绝对路径或相对于工作目录
	dataPath := config.DataPath
	if !filepath.IsAbs(dataPath) {
		absPath, err := filepath.Abs(dataPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path: %w", err)
		}
		dataPath = absPath
	}

	// 设置存储选项
	storage.SetDedupInterval(config.DedupInterval)
	storage.SetFreeDiskSpaceLimit(config.FreeDiskSpaceLimitBytes())
	storage.SetDataFlushInterval(5 * time.Second)

	// 打开存储
	opts := storage.OpenOptions{
		Retention: time.Duration(config.RetentionDays) * 24 * time.Hour,
	}

	stor := storage.MustOpenStorage(dataPath, opts)

	db := &TSDB{
		storage: stor,
		config:  config,
	}

	log.Printf("NEZHA>> TSDB opened at %s, retention: %d days, max disk: %.1f GB",
		dataPath, config.RetentionDays, config.MaxDiskUsageGB)

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
	db.storage.DebugFlush()
}
