DO $$
BEGIN
  IF to_regclass('public.hotmethod_tasks') IS NOT NULL THEN
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS task_kind varchar(64);
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS request_id varchar(64);
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS deadline_unix_ms bigint DEFAULT 0;
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS resource_budget jsonb;
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS cancel_requested boolean DEFAULT false;
    ALTER TABLE hotmethod_tasks ADD COLUMN IF NOT EXISTS canceled_at timestamptz;
    CREATE INDEX IF NOT EXISTS idx_hotmethod_tasks_task_kind ON hotmethod_tasks(task_kind);
    CREATE INDEX IF NOT EXISTS idx_hotmethod_tasks_request_id ON hotmethod_tasks(request_id);
    CREATE INDEX IF NOT EXISTS idx_hotmethod_tasks_cancel_requested ON hotmethod_tasks(cancel_requested);
  END IF;

  IF to_regclass('public.agent_infos') IS NOT NULL THEN
    ALTER TABLE agent_infos ADD COLUMN IF NOT EXISTS capabilities jsonb;
    ALTER TABLE agent_infos ADD COLUMN IF NOT EXISTS supported_os varchar(64);
  END IF;

  IF to_regclass('public.task_status_events') IS NOT NULL THEN
    ALTER TABLE task_status_events ADD COLUMN IF NOT EXISTS sequence bigint DEFAULT 0;
    ALTER TABLE task_status_events ADD COLUMN IF NOT EXISTS attempt_id bigint DEFAULT 0;
    ALTER TABLE task_status_events ADD COLUMN IF NOT EXISTS source_module varchar(64);
    ALTER TABLE task_status_events ADD COLUMN IF NOT EXISTS payload jsonb;
    CREATE INDEX IF NOT EXISTS idx_task_status_events_sequence ON task_status_events(sequence);
    CREATE INDEX IF NOT EXISTS idx_task_status_events_attempt_id ON task_status_events(attempt_id);
  END IF;

  IF to_regclass('public.artifacts') IS NOT NULL THEN
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS etag varchar(128);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS hash varchar(128);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS manifest_key varchar(512);
    ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS retention varchar(64);
  END IF;

  IF to_regclass('public.analysis_jobs') IS NOT NULL THEN
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS max_attempts integer DEFAULT 3;
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS last_error varchar(1024);
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS input_artifact_ids jsonb;
    ALTER TABLE analysis_jobs ADD COLUMN IF NOT EXISTS output_artifact_ids jsonb;
  END IF;

  IF to_regclass('public.schedule_tasks') IS NOT NULL THEN
    ALTER TABLE schedule_tasks ADD COLUMN IF NOT EXISTS task_kind varchar(64);
  END IF;

  IF to_regclass('public.outboxes') IS NOT NULL THEN
    ALTER TABLE outboxes ADD COLUMN IF NOT EXISTS status varchar(32) DEFAULT 'pending';
    ALTER TABLE outboxes ADD COLUMN IF NOT EXISTS locked_at timestamptz;
    ALTER TABLE outboxes ADD COLUMN IF NOT EXISTS locked_by varchar(128);
    ALTER TABLE outboxes ADD COLUMN IF NOT EXISTS dead_letter_at timestamptz;
    CREATE INDEX IF NOT EXISTS idx_outbox_claim_v2 ON outboxes(status, next_attempt_at, locked_at, created_at);
  END IF;
END $$;
