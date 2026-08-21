-- SQL migrations run before GORM AutoMigrate. On a fresh database the Session
-- table does not exist yet, so only alter/archive it when upgrading an existing
-- installation. AutoMigrate creates the complete table immediately afterwards.
DO $$
BEGIN
    IF to_regclass('public.continuous_sessions') IS NOT NULL THEN
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS scope varchar(16) DEFAULT 'host';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS selector_exe varchar(4096) DEFAULT '';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS selector_mode varchar(32) DEFAULT 'all_instances';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS signals jsonb DEFAULT '["cpu_profile","io_latency","io_syscall_latency","sched_latency"]'::jsonb;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS desired_state varchar(16) DEFAULT 'running';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS observed_state varchar(16) DEFAULT 'pending';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS observed_at timestamptz;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS active_processes jsonb DEFAULT '[]'::jsonb;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS continuity_mode varchar(16) DEFAULT 'degraded';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS allow_degraded boolean DEFAULT false;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS degradation_reason text DEFAULT '';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS last_error text DEFAULT '';
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS stop_requested_at timestamptz;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS revision bigint DEFAULT 1;
        ALTER TABLE continuous_sessions ADD COLUMN IF NOT EXISTS agent_id varchar(128) DEFAULT '';

        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_scope ON continuous_sessions(scope);
        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_desired_state ON continuous_sessions(desired_state);
        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_observed_state ON continuous_sessions(observed_state);
        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_observed_at ON continuous_sessions(observed_at);
        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_agent_id ON continuous_sessions(agent_id);
        CREATE INDEX IF NOT EXISTS idx_continuous_sessions_selector_exe ON continuous_sessions(selector_exe);

        -- Sessions produced by the old Agent-start path are history, not desired work.
        UPDATE continuous_sessions
        SET status = 'stopped',
            desired_state = 'stopped',
            observed_state = 'stopped',
            continuity_mode = 'legacy',
            degradation_reason = CASE WHEN degradation_reason = '' THEN 'archived during user-driven continuous migration' ELSE degradation_reason END,
            stopped_at = COALESCE(stopped_at, last_upload_at, updated_at, created_at),
            stop_requested_at = COALESCE(stop_requested_at, last_upload_at, updated_at, created_at),
            updated_at = NOW()
        WHERE status = 'running'
          AND (desired_state IS NULL OR desired_state = 'running')
          AND (agent_id IS NULL OR agent_id = '');
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS continuous_process_snapshots (
    id bigserial PRIMARY KEY,
    target_ip varchar(45) NOT NULL,
    agent_id varchar(128) DEFAULT '',
    pid integer NOT NULL,
    process_start_ms bigint NOT NULL,
    comm varchar(256) DEFAULT '',
    exe varchar(4096) DEFAULT '',
    rss_bytes bigint DEFAULT 0,
    observed_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_continuous_process_identity
    ON continuous_process_snapshots(target_ip, pid, process_start_ms);
CREATE INDEX IF NOT EXISTS idx_continuous_process_agent_id ON continuous_process_snapshots(agent_id);
CREATE INDEX IF NOT EXISTS idx_continuous_process_exe ON continuous_process_snapshots(exe);
CREATE INDEX IF NOT EXISTS idx_continuous_process_observed_at ON continuous_process_snapshots(observed_at);

CREATE TABLE IF NOT EXISTS continuous_agent_states (
    id bigserial PRIMARY KEY,
    target_ip varchar(45) NOT NULL UNIQUE,
    agent_id varchar(128) DEFAULT '',
    strict_capable boolean DEFAULT false,
    capabilities jsonb DEFAULT '[]'::jsonb,
    revision bigint DEFAULT 0,
    observed_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_continuous_agent_state_agent_id ON continuous_agent_states(agent_id);
CREATE INDEX IF NOT EXISTS idx_continuous_agent_state_observed_at ON continuous_agent_states(observed_at);
