CREATE INDEX idx_audit_runtime_host_path
    ON audit_log(
        user_id,
        service,
        COALESCE(json_extract(params_safe, '$.host'), ''),
        COALESCE(json_extract(params_safe, '$.path'), '')
    );
