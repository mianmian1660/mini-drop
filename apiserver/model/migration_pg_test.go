// ============================================================
// server/migration_pg_test.go — 阶段五 016/017/018 迁移幂等测试（PostgreSQL）
// ============================================================
// 仅在设置了 TEST_PG_DSN 时运行（服务器 E2E）：对真实 PG 数据库应用
// 016/017 两次，验证首次应用与重复应用都幂等成功；并验证部分唯一索引
// 的 active 唯一性约束。
// ============================================================

package model

import (
	"fmt"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func pgTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN 未设置，跳过 PostgreSQL 迁移测试")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	return db
}

func TestMigrations016018Idempotent(t *testing.T) {
	db := pgTestDB(t)
	if err := db.Exec("CREATE SCHEMA IF NOT EXISTS public").Error; err != nil {
		t.Fatal(err)
	}
	// 生产升级库已有业务表；独立临时库先构造同等基线，再验证 SQL 迁移。
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("构造迁移前 schema 失败: %v", err)
	}
	// 首次应用全部迁移（含 016/017）
	if err := RunMigrations(db); err != nil {
		t.Fatalf("首次 RunMigrations 失败: %v", err)
	}
	// 重复应用必须幂等
	if err := RunMigrations(db); err != nil {
		t.Fatalf("重复 RunMigrations 失败: %v", err)
	}

	// 三张 v2 账本表存在
	for _, table := range []string{"continuous_parquet_blocks", "continuous_parquet_block_files", "continuous_parquet_block_members"} {
		var count int64
		if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=?", table).Scan(&count).Error; err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if count == 0 {
			t.Fatalf("迁移后表 %s 不存在", table)
		}
	}

	// 部分唯一索引 uq_cpq_active_partition 存在
	var indexCount int64
	if err := db.Raw("SELECT COUNT(*) FROM pg_indexes WHERE schemaname='public' AND indexname='uq_cpq_active_partition'").Scan(&indexCount).Error; err != nil {
		t.Fatal(err)
	}
	if indexCount == 0 {
		t.Fatal("uq_cpq_active_partition 部分唯一索引未创建")
	}

	// 两个 active 同一分区 → 第二个应失败
	err := db.Exec(`
		INSERT INTO continuous_parquet_blocks
		(block_id, tenant, bucket_start, bucket_end, signal_type, resolution, version, status, validation)
		VALUES ('pg-test-a', 'default', '2026-08-23T10:00:00Z', '2026-08-23T11:00:00Z', 'cpu', 'raw', 1, 'active', 'passed')`).Error
	if err != nil {
		t.Fatalf("插入 active 块失败: %v", err)
	}
	err = db.Exec(`
		INSERT INTO continuous_parquet_blocks
		(block_id, tenant, bucket_start, bucket_end, signal_type, resolution, version, status, validation)
		VALUES ('pg-test-b', 'default', '2026-08-23T10:00:00Z', '2026-08-23T11:00:00Z', 'cpu', 'raw', 2, 'active', 'passed')`).Error
	if err == nil {
		t.Fatal("同一分区第二个 active 应被部分唯一索引拒绝")
	}
	// active 但 validation!=passed 必须由数据库 CHECK 拒绝。
	err = db.Exec(`
		INSERT INTO continuous_parquet_blocks
		(block_id, tenant, bucket_start, bucket_end, signal_type, resolution, version, status, validation)
		VALUES ('pg-test-invalid', 'default', '2026-08-23T12:00:00Z', '2026-08-23T13:00:00Z', 'cpu', 'raw', 1, 'active', 'pending')`).Error
	if err == nil {
		t.Fatal("active/pending 应被数据库约束拒绝")
	}
	// 清理测试行
	_ = db.Exec("DELETE FROM continuous_parquet_blocks WHERE block_id IN ('pg-test-a','pg-test-b')").Error
}

func TestMigrations016ObjectKeyUnique(t *testing.T) {
	db := pgTestDB(t)
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("构造 schema 失败: %v", err)
	}
	if err := RunMigrations(db); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	for _, id := range []string{"pg-file-a", "pg-file-b"} {
		if err := db.Exec(`
			INSERT INTO continuous_parquet_blocks
			(block_id, tenant, bucket_start, bucket_end, signal_type, resolution, version, status, validation)
			VALUES (?, 'default', '2026-08-24T10:00:00Z', '2026-08-24T11:00:00Z', 'cpu', 'raw', 1, 'failed', 'failed')
			ON CONFLICT (block_id) DO NOTHING`, id).Error; err != nil {
			t.Fatalf("插入父 block 失败: %v", err)
		}
	}
	err := db.Exec(`
		INSERT INTO continuous_parquet_block_files (block_id, part_index, object_key)
		VALUES ('pg-file-a', 0, 'continuous/v2/default/date=2026-08-23/hour=10/signal=cpu/resolution=raw/pg-file-a-00.parquet')`).Error
	if err != nil {
		t.Fatalf("插入 block_file 失败: %v", err)
	}
	err = db.Exec(`
		INSERT INTO continuous_parquet_block_files (block_id, part_index, object_key)
		VALUES ('pg-file-b', 0, 'continuous/v2/default/date=2026-08-23/hour=10/signal=cpu/resolution=raw/pg-file-a-00.parquet')`).Error
	if err == nil {
		t.Fatal("重复 object_key 应被唯一索引拒绝")
	}
	_ = db.Exec("DELETE FROM continuous_parquet_block_files WHERE block_id IN ('pg-file-a','pg-file-b')").Error
	_ = db.Exec("DELETE FROM continuous_parquet_blocks WHERE block_id IN ('pg-file-a','pg-file-b')").Error
}

// 026 主机元数据迁移：agent_infos.host_metadata JSONB 列存在且可读写
func TestMigration026HostMetadata(t *testing.T) {
	db := pgTestDB(t)
	if err := AutoMigrate(db); err != nil {
		t.Fatalf("构造 schema 失败: %v", err)
	}
	if err := RunMigrations(db); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	// 重复应用必须幂等
	if err := RunMigrations(db); err != nil {
		t.Fatalf("重复 RunMigrations 失败: %v", err)
	}

	// 列存在
	var colCount int64
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name='agent_infos' AND column_name='host_metadata'`).Scan(&colCount).Error; err != nil {
		t.Fatal(err)
	}
	if colCount == 0 {
		t.Fatal("agent_infos.host_metadata 列未创建")
	}

	// JSONB 读写
	metaJSON := `{"os_name":"Ubuntu","os_version":"24.04","kernel_version":"6.8.0-31-generic","architecture":"x86_64","cpu_model":"AMD EPYC 7B12","cpu_cores":8,"uptime_seconds":86400}`
	if err := db.Exec(`INSERT INTO agent_infos (hostname, ip_addr, online, host_metadata)
		VALUES ('pg-meta-host', '10.9.9.9', true, ?::jsonb)`, metaJSON).Error; err != nil {
		t.Fatalf("写入 host_metadata 失败: %v", err)
	}
	var stored string
	if err := db.Raw(`SELECT host_metadata::text FROM agent_infos WHERE ip_addr='10.9.9.9'`).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		t.Fatal("host_metadata 读取为空")
	}
	_ = db.Exec("DELETE FROM agent_infos WHERE ip_addr='10.9.9.9'").Error
}

var _ = fmt.Sprintf
