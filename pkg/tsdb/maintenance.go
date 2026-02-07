package tsdb

import (
	"log"
)

func (db *TSDB) Maintenance() {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return
	}

	log.Println("NEZHA>> TSDB starting maintenance (flush)...")
	db.storage.DebugFlush()
	log.Println("NEZHA>> TSDB maintenance completed")
}
