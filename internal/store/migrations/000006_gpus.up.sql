-- M1 ticket #57: whole-GPU allocation (ADR-0015).

-- The Node's per-GPU CPU and memory share, recomputed from the heartbeat:
-- (allocatable x 3/4) / card count (defaults.md).
ALTER TABLE nodes
    ADD COLUMN cpu_per_gpu double precision NOT NULL DEFAULT 0,
    ADD COLUMN memory_per_gpu bigint NOT NULL DEFAULT 0;

-- The requested model is a placement constraint, so it must survive a restart.
ALTER TABLE sandboxes
    ADD COLUMN gpu_model text;

-- The primary key is the exclusivity guarantee: one Sandbox per GPU. The
-- foreign key follows the RESTRICT convention (schema.md); ReleaseCompute
-- deletes the claims before the Allocation they belong to. There is no foreign
-- key to gpus: a Capabilities re-report replaces those rows while the cards a
-- running Sandbox holds stay allocated.
CREATE TABLE gpu_allocations (
    gpu_uuid   text PRIMARY KEY,
    sandbox_id uuid NOT NULL REFERENCES allocations (sandbox_id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX gpu_allocations_sandbox_id ON gpu_allocations (sandbox_id);
