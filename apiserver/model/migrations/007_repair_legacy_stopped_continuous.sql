-- 006 added desired/observed columns with running/pending defaults. Historical
-- sessions that were already stopped therefore inherited an active desired
-- state even though their compatibility status and stopped_at were final.
-- Restrict the repair to pre-control-plane rows with no owning Agent. SQL
-- migrations run before AutoMigrate, so a fresh database has no Session table.
DO $$
BEGIN
    IF to_regclass('public.continuous_sessions') IS NOT NULL THEN
        UPDATE continuous_sessions
        SET desired_state = 'stopped',
            observed_state = 'stopped',
            continuity_mode = 'legacy',
            degradation_reason = CASE
                WHEN degradation_reason = '' THEN 'archived during user-driven continuous migration'
                ELSE degradation_reason
            END,
            stop_requested_at = COALESCE(stop_requested_at, stopped_at, last_upload_at, updated_at, created_at),
            updated_at = NOW()
        WHERE status = 'stopped'
          AND desired_state = 'running'
          AND observed_state = 'pending'
          AND stopped_at IS NOT NULL
          AND (agent_id IS NULL OR agent_id = '');
    END IF;
END $$;
