package singleton

import (
	"fmt"
	"log"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/nezhahq/nezha/model"
)

// Migrate migrates SQLite data to the currently configured database
func Migrate(sqlitePath string) error {
	if Conf.DB.Type == "sqlite" || Conf.DB.Type == "" {
		return fmt.Errorf("target database cannot be SQLite, please configure MySQL or PostgreSQL in the config file first")
	}

	if DB == nil {
		return fmt.Errorf("target database not initialized")
	}

	sourceDB, err := gorm.Open(sqlite.Open(sqlitePath), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("failed to open source SQLite database: %v", err)
	}
	// Ensure source SQLite database connection is closed after migration to release file handle promptly
	sourceSQLDB, err := sourceDB.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying source SQLite database connection: %v", err)
	}
	defer sourceSQLDB.Close()

	log.Println("NEZHA>> Migrating data to new database...")

	// Use transaction to ensure migration atomicity
	err = DB.Transaction(func(tx *gorm.DB) error {
		// Migrate tables in dependency order
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

	log.Println("NEZHA>> Data migration completed!")
	return nil
}

func migrateTable[T any](source, dest *gorm.DB, model T) error {
	log.Printf("NEZHA>> Migrating table: %T", model)

	// Read and write in batches to prevent memory overflow.
	// Use keyset pagination on the primary key (assumed to be "id") instead of OFFSET-based pagination
	// to avoid skipping or duplicating records if the source table is modified during migration.
	batchSize := 100
	var lastID int64

	for {
		// Fetch the next batch of primary keys in a stable order.
		var ids []int64
		if err := source.Model(model).
			Where("id > ?", lastID).
			Order("id").
			Limit(batchSize).
			Pluck("id", &ids).Error; err != nil {
			return fmt.Errorf("failed to fetch primary keys for table %T: %v", model, err)
		}

		if len(ids) == 0 {
			// No more records to migrate.
			break
		}

		// Fetch the actual records for this batch of IDs, ordered by id for determinism.
		var results []T
		if err := source.Where("id IN ?", ids).Order("id").Find(&results).Error; err != nil {
			return fmt.Errorf("failed to read model %T: %v", model, err)
		}

		if len(results) > 0 {
			if err := dest.Create(&results).Error; err != nil {
				return fmt.Errorf("failed to write model %T: %v", model, err)
			}
		}

		// Advance the cursor to the highest ID we've processed so far.
		lastID = ids[len(ids)-1]
	}
	return nil
}
