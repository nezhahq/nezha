package singleton

import (
	"log"
	"time"

	"github.com/nezhahq/nezha/pkg/tsdb"
)

var TSDBShared *tsdb.TSDB

func InitTSDB() error {
	config := &tsdb.Config{
		RetentionDays:      30,
		MinFreeDiskSpaceGB: 1,
		MaxMemoryMB:        256,
	}

	if Conf.TSDB.DataPath != "" {
		config.DataPath = Conf.TSDB.DataPath
	}
	if Conf.TSDB.RetentionDays > 0 {
		config.RetentionDays = Conf.TSDB.RetentionDays
	}
	if Conf.TSDB.MinFreeDiskSpaceGB > 0 {
		config.MinFreeDiskSpaceGB = Conf.TSDB.MinFreeDiskSpaceGB
	}
	if Conf.TSDB.MaxMemoryMB > 0 {
		config.MaxMemoryMB = Conf.TSDB.MaxMemoryMB
	}
	if Conf.TSDB.WriteBufferSize > 0 {
		config.WriteBufferSize = Conf.TSDB.WriteBufferSize
	}
	if Conf.TSDB.WriteBufferFlushInterval > 0 {
		config.WriteBufferFlushInterval = time.Duration(Conf.TSDB.WriteBufferFlushInterval) * time.Second
	}

	if !config.Enabled() {
		log.Println("NEZHA>> TSDB is disabled (tsdb.data_path not configured)")
		return nil
	}

	var err error
	TSDBShared, err = tsdb.Open(config)
	if err != nil {
		return err
	}

	log.Println("NEZHA>> TSDB initialized successfully")

	if DB != nil && DB.Migrator().HasTable("service_histories") {
		log.Println("NEZHA>> Dropping legacy service_histories table (TSDB is now enabled). Historical data will NOT be migrated.")
		if err := DB.Migrator().DropTable("service_histories"); err != nil {
			log.Printf("NEZHA>> Warning: failed to drop service_histories table: %v", err)
		}
	}

	return nil
}

func TSDBEnabled() bool {
	return TSDBShared != nil && !TSDBShared.IsClosed()
}

func CloseTSDB() {
	if TSDBShared != nil {
		TSDBShared.Close()
	}
}
