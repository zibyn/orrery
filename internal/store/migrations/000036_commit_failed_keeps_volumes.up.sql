-- A stop whose commit failed leaves uncommitted Volume writes on the Node:
-- the failed Sandbox keeps its Volumes until it is deleted, so no other
-- Sandbox mounts the older head meanwhile (runtime.md §17).
CREATE OR REPLACE FUNCTION sandbox_volumes_follow_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('pending', 'provisioning', 'ready', 'stopping') THEN
        UPDATE sandbox_volumes SET held = true WHERE sandbox_id = NEW.id AND NOT held;
    ELSIF NEW.state = 'failed' AND NEW.cause IS NOT DISTINCT FROM 'commit_failed' THEN
        NULL;
    ELSIF NEW.state IN ('stopped', 'failed', 'deleted') THEN
        UPDATE sandbox_volumes SET held = false WHERE sandbox_id = NEW.id AND held;
    END IF;
    RETURN NULL;
END;
$$;
