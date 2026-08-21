-- Persist the user's fallback policy so the Agent can enforce it when a
-- runtime perf/CO-RE attach fails after the initial capability advertisement.
DO $$
BEGIN
    IF to_regclass('public.continuous_sessions') IS NOT NULL THEN
        ALTER TABLE continuous_sessions
            ADD COLUMN IF NOT EXISTS allow_degraded boolean DEFAULT false;
    END IF;
END $$;
