package model

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type SchemaMigration struct {
	Version   string    `gorm:"column:version;primaryKey;size:128" json:"version"`
	AppliedAt time.Time `gorm:"column:applied_at" json:"applied_at"`
}

func (SchemaMigration) TableName() string {
	return "schema_migrations"
}

func RunMigrations(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		return err
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var count int64
		if err := db.Model(&SchemaMigration{}).Where("version = ?", version).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		if db.Dialector.Name() == "postgres" {
			sqlBytes, err := migrationFiles.ReadFile(path.Join("migrations", name))
			if err != nil {
				return err
			}
			if err := db.Exec(string(sqlBytes)).Error; err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
		}
		if err := db.Create(&SchemaMigration{Version: version, AppliedAt: time.Now()}).Error; err != nil {
			return err
		}
	}
	return nil
}
