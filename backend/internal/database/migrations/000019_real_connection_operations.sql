-- 真实对接的幂等意图必须在任何远端副作用前占位。
CREATE TABLE IF NOT EXISTS real_connection_operations (
    id text PRIMARY KEY DEFAULT md5(random()::text || clock_timestamp()::text),
    user_id text NOT NULL,
    workspace_admin_account_id text NOT NULL,
    operation_id text NOT NULL,
    request_hash text NOT NULL,
    owner_token text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'reserved',
    connection_id text NOT NULL DEFAULT '',
    remote_upstream_key_id text NOT NULL DEFAULT '',
    remote_admin_resource_id text NOT NULL DEFAULT '',
    last_error text NOT NULL DEFAULT '',
    compensation_error text NOT NULL DEFAULT '',
    attempt_count integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT real_connection_operations_unique_intent UNIQUE (user_id, workspace_admin_account_id, operation_id),
    CONSTRAINT real_connection_operations_status_check CHECK (status IN ('reserved', 'provisioning', 'succeeded', 'failed', 'compensation_failed'))
);

CREATE INDEX IF NOT EXISTS idx_real_connection_operations_status
    ON real_connection_operations (user_id, workspace_admin_account_id, status);

-- 旧版本只在 real_connections 写入 operation_id。将这些已完成记录导入审计表，
-- 避免滚动升级时重试同一个 operation_id 再次触发远端创建。
DO $$
BEGIN
    IF to_regclass('public.real_connections') IS NOT NULL THEN
        INSERT INTO real_connection_operations (
            user_id, workspace_admin_account_id, operation_id, request_hash,
            owner_token, status, connection_id, remote_upstream_key_id,
            remote_admin_resource_id, created_at, updated_at
        )
        SELECT
            user_id,
            workspace_admin_account_id,
            operation_id,
            '',
            '',
            'succeeded',
            id,
            upstream_key_id,
            admin_account_id,
            created_at,
            created_at
        FROM real_connections
        WHERE operation_id <> ''
        ON CONFLICT (user_id, workspace_admin_account_id, operation_id) DO NOTHING;
    END IF;
END
$$;
