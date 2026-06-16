// Package database opens and migrates application database connections.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

// DB wraps the application write/read GORM pools.
type DB struct {
	Write *gorm.DB
	Read  *gorm.DB

	pools *gormdb.Pools
}

// Config controls New.
type Config struct {
	Driver  gormdb.Driver
	DSN     string
	ReadDSN string
	Logger  logger.Interface
}

// New opens database pools using the shared GORM database opener.
func New(cfg Config) (*DB, error) {
	pools, err := gormdb.Open(gormdb.Config{
		Driver:  cfg.Driver,
		DSN:     cfg.DSN,
		ReadDSN: cfg.ReadDSN,
		Logger:  cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &DB{
		Write: pools.Write,
		Read:  pools.Read,
		pools: pools,
	}, nil
}

// Migrate runs all application migrations.
func (db *DB) Migrate(ctx context.Context) error {
	write := db.Write.WithContext(ctx)
	if err := dropLegacyTenantColumns(write); err != nil {
		return err
	}
	if err := write.AutoMigrate(model.AllModels()...); err != nil {
		return err
	}
	return write.AutoMigrate(store.JobModels()...)
}

func dropLegacyTenantColumns(db *gorm.DB) error {
	for _, table := range []string{
		"users",
		"projects",
		"server_state",
		"sandbox_access_issuer_keys",
		"agent_configs",
		"sandboxes",
		"sandbox_provider_instances",
		"workers",
		"worker_bootstrap_tokens",
		"worker_auth_tokens",
		"project_events",
		"jobqueue_jobs",
		"jobqueue_leaders",
	} {
		if !db.Migrator().HasColumn(table, "tenant_id") {
			continue
		}
		if err := dropLegacyTenantIndexes(db, table); err != nil {
			return err
		}
		if db.Name() == "sqlite" {
			primaryKeyColumn, err := sqlitePrimaryKeyColumn(db, table, "tenant_id")
			if err != nil {
				return err
			}
			if primaryKeyColumn {
				if err := rebuildSQLiteTableWithoutColumn(db, table, "tenant_id"); err != nil {
					return err
				}
				continue
			}
		}
		if err := db.Exec(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", quoteIdentifier(table), quoteIdentifier("tenant_id"))).Error; err != nil {
			if db.Name() == "sqlite" {
				if rebuildErr := rebuildSQLiteTableWithoutColumn(db, table, "tenant_id"); rebuildErr == nil {
					continue
				}
			}
			return err
		}
	}
	return nil
}

func dropLegacyTenantIndexes(db *gorm.DB, table string) error {
	if db.Name() != "sqlite" {
		return nil
	}

	var indexes []struct {
		Name   string
		Origin string
	}
	if err := db.Raw("PRAGMA index_list(" + quoteIdentifier(table) + ")").Scan(&indexes).Error; err != nil {
		return err
	}
	for _, index := range indexes {
		if index.Origin == "pk" || strings.HasPrefix(index.Name, "sqlite_autoindex_") {
			continue
		}
		var columns []struct {
			Name string
		}
		if err := db.Raw("PRAGMA index_info(" + quoteIdentifier(index.Name) + ")").Scan(&columns).Error; err != nil {
			return err
		}
		for _, column := range columns {
			if column.Name != "tenant_id" {
				continue
			}
			if err := db.Exec("DROP INDEX IF EXISTS " + quoteIdentifier(index.Name)).Error; err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func sqlitePrimaryKeyColumn(db *gorm.DB, table, column string) (bool, error) {
	var columns []sqliteColumn
	if err := db.Raw("PRAGMA table_info(" + quoteIdentifier(table) + ")").Scan(&columns).Error; err != nil {
		return false, err
	}
	for _, current := range columns {
		if current.Name == column {
			return current.PK != 0, nil
		}
	}
	return false, nil
}

func rebuildSQLiteTableWithoutColumn(db *gorm.DB, table, droppedColumn string) error {
	var columns []sqliteColumn
	if err := db.Raw("PRAGMA table_info(" + quoteIdentifier(table) + ")").Scan(&columns).Error; err != nil {
		return err
	}

	kept := make([]sqliteColumn, 0, len(columns))
	for _, column := range columns {
		if column.Name != droppedColumn {
			kept = append(kept, column)
		}
	}
	if len(kept) == len(columns) {
		return nil
	}
	if len(kept) == 0 {
		return fmt.Errorf("cannot rebuild %s after dropping only column %s", table, droppedColumn)
	}

	tempTable := "__discobox_migrate_" + table
	columnDefs := make([]string, 0, len(kept)+1)
	pkColumns := make([]string, 0, len(kept))
	columnNames := make([]string, 0, len(kept))
	for _, column := range kept {
		columnNames = append(columnNames, quoteIdentifier(column.Name))
		def := quoteIdentifier(column.Name)
		if strings.TrimSpace(column.Type) != "" {
			def += " " + column.Type
		}
		if column.NotNull != 0 && column.PK == 0 {
			def += " NOT NULL"
		}
		if column.Default.Valid {
			def += " DEFAULT " + column.Default.String
		}
		columnDefs = append(columnDefs, def)
		if column.PK != 0 {
			pkColumns = append(pkColumns, quoteIdentifier(column.Name))
		}
	}
	if len(pkColumns) > 0 {
		columnDefs = append(columnDefs, "PRIMARY KEY ("+strings.Join(pkColumns, ", ")+")")
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DROP TABLE IF EXISTS " + quoteIdentifier(tempTable)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf("CREATE TABLE %s (%s)", quoteIdentifier(tempTable), strings.Join(columnDefs, ", "))).Error; err != nil {
			return err
		}
		columnsSQL := strings.Join(columnNames, ", ")
		if err := tx.Exec(fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", quoteIdentifier(tempTable), columnsSQL, columnsSQL, quoteIdentifier(table))).Error; err != nil {
			return err
		}
		if err := tx.Exec("DROP TABLE " + quoteIdentifier(table)).Error; err != nil {
			return err
		}
		return tx.Exec(fmt.Sprintf("ALTER TABLE %s RENAME TO %s", quoteIdentifier(tempTable), quoteIdentifier(table))).Error
	})
}

type sqliteColumn struct {
	CID     int            `gorm:"column:cid"`
	Name    string         `gorm:"column:name"`
	Type    string         `gorm:"column:type"`
	NotNull int            `gorm:"column:notnull"`
	Default sql.NullString `gorm:"column:dflt_value"`
	PK      int            `gorm:"column:pk"`
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Close closes the underlying database pools.
func (db *DB) Close() error {
	if db == nil || db.pools == nil {
		return nil
	}
	return db.pools.Close()
}
