-- 004_continuous_dual_track.sql — continuous CPU profile + latency histogram signals

ALTER TABLE profile_batches
    ADD COLUMN IF NOT EXISTS schema_version integer NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS signal_types jsonb,
    ADD COLUMN IF NOT EXISTS backends jsonb;

ALTER TABLE profile_windows
    ADD COLUMN IF NOT EXISTS signal_type varchar(64) NOT NULL DEFAULT 'cpu_profile',
    ADD COLUMN IF NOT EXISTS schema_version integer NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS backend varchar(64);

CREATE INDEX IF NOT EXISTS idx_profile_windows_signal_type ON profile_windows(signal_type);
CREATE INDEX IF NOT EXISTS idx_profile_windows_session_signal_time
    ON profile_windows(session_sid, signal_type, window_start);
