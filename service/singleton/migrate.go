package singleton

import (
	"fmt"
	"log"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// Migrate 将 SQLite 数据迁移到当前配置的数据库
func Migrate(sqlitePath string) error {
	if Conf.DB.Type == "sqlite" || Conf.DB.Type == "" {
		return fmt.Errorf("目标数据库不能是 SQLite，请先在配置文件中配置 MySQL 或 PostgreSQL")
	}

	sourceDB, err := gorm.Open(sqlite.Open(sqlitePath), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("打开源 SQLite 数据库失败: %v", err)
	}

	log.Println("NEZHA>> 正在迁移数据到新数据库...")

	// 使用事务确保迁移的原子性
	err = DB.Transaction(func(tx *gorm.DB) error {
		// 按照依赖顺序迁移表
		if err := migrateTable(sourceDB, tx, &model.User{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Server{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.ServerGroup{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.ServerGroupServer{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.NotificationGroup{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Notification{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.NotificationGroupNotification{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.AlertRule{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Service{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.ServiceHistory{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Cron{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Transfer{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.NAT{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.DDNSProfile{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.WAF{}); err != nil {
			return err
		}
		if err := migrateTable(sourceDB, tx, &model.Oauth2Bind{}); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	log.Println("NEZHA>> 数据迁移完成！")
	return nil
}

func migrateTable[T any](source, dest *gorm.DB, model T) error {
	var results []T
	log.Printf("NEZHA>> 正在迁移表: %v", fmt.Sprintf("%T", model))

	// 分批读取和写入，防止内存溢出
	batchSize := 100
	var count int64
	source.Model(model).Count(&count)

	for i := 0; i < int(count); i += batchSize {
		if err := source.Offset(i).Limit(batchSize).Find(&results).Error; err != nil {
			return fmt.Errorf("读取模型 %T 失败: %v", model, err)
		}
		if len(results) > 0 {
			if err := dest.Create(&results).Error; err != nil {
				return fmt.Errorf("写入模型 %T 失败: %v", model, err)
			}
		}
	}
	return nil
}
