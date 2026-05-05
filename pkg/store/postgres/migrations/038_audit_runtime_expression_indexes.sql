CREATE INDEX idx_audit_runtime_host_path
    ON audit_log(
        user_id,
        service,
        COALESCE(params_safe->>'host', ''),
        COALESCE(params_safe->>'path', '')
    );
