-- 004_continuous_dual_track.sql — continuous CPU profile + latency histogram signals
--
-- profile_batches/profile_windows 是 GORM AutoMigrate（main.go 里排在
-- RunMigrations 之后）才会建出来的表，所以这里的 ALTER 必须和
-- 001_stage1_contract.sql 一样用 to_regclass 存在性判断包一层——
-- 全新初始化的数据库第一次跑到这里时这两张表还不存在，直接裸 ALTER
-- 会报 "relation does not exist" 并让 apiserver 启动失败。表不存在就
-- 安全跳过，等 AutoMigrate 建完表后，下次重启再跑一遍迁移就能正常补列。

DO $$
BEGIN
  IF to_regclass('public.profile_batches') IS NOT NULL THEN
    ALTER TABLE profile_batches
        ADD COLUMN IF NOT EXISTS schema_version integer NOT NULL DEFAULT 1,
        ADD COLUMN IF NOT EXISTS signal_types jsonb,
        ADD COLUMN IF NOT EXISTS backends jsonb;
  END IF;

  IF to_regclass('public.profile_windows') IS NOT NULL THEN
    ALTER TABLE profile_windows
        ADD COLUMN IF NOT EXISTS signal_type varchar(64) NOT NULL DEFAULT 'cpu_profile',
        ADD COLUMN IF NOT EXISTS schema_version integer NOT NULL DEFAULT 1,
        ADD COLUMN IF NOT EXISTS backend varchar(64);

    CREATE INDEX IF NOT EXISTS idx_profile_windows_signal_type ON profile_windows(signal_type);
    CREATE INDEX IF NOT EXISTS idx_profile_windows_session_signal_time
        ON profile_windows(session_sid, signal_type, window_start);
  END IF;
END $$;
