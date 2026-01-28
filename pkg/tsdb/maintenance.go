package tsdb

import (
	"log"
)

// Maintenance 执行 TSDB 维护操作
func (db *TSDB) Maintenance() {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return
	}

	log.Println("NEZHA>> TSDB starting maintenance (flush and optimization)...")
	// 强制刷盘，VictoriaMetrics 会在刷盘过程中处理部分数据合并
	db.storage.DebugFlush()
	log.Println("NEZHA>> TSDB maintenance completed")
}
