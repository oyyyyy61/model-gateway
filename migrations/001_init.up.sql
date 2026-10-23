CREATE TABLE tenants (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    status VARCHAR(32) NOT NULL,
    rate_limit INT NOT NULL,
    rate_window_sec INT NOT NULL,
    max_inflight INT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL
);

CREATE TABLE api_keys (
    id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    key_prefix VARCHAR(32) NOT NULL,
    key_hash CHAR(64) NOT NULL UNIQUE,
    status VARCHAR(32) NOT NULL,
    last_used_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    INDEX idx_api_keys_tenant (tenant_id),
    CONSTRAINT fk_api_keys_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id)
);

CREATE TABLE models (
    id VARCHAR(64) PRIMARY KEY,
    name VARCHAR(128) NOT NULL UNIQUE,
    protocol VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL
);

CREATE TABLE tenant_models (
    tenant_id VARCHAR(64) NOT NULL,
    model_id VARCHAR(64) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (tenant_id, model_id),
    CONSTRAINT fk_tenant_models_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT fk_tenant_models_model FOREIGN KEY (model_id) REFERENCES models (id)
);

CREATE TABLE model_instances (
    id VARCHAR(64) PRIMARY KEY,
    model_id VARCHAR(64) NOT NULL,
    endpoint VARCHAR(512) NOT NULL,
    weight INT NOT NULL,
    max_inflight INT NOT NULL,
    status VARCHAR(32) NOT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    INDEX idx_model_instances_model (model_id),
    CONSTRAINT fk_model_instances_model FOREIGN KEY (model_id) REFERENCES models (id)
);

CREATE TABLE request_audits (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    request_id VARCHAR(64) NOT NULL,
    tenant_id VARCHAR(64) NOT NULL,
    model_id VARCHAR(64) NOT NULL,
    instance_id VARCHAR(64) NULL,
    stream BOOLEAN NOT NULL,
    status_code INT NOT NULL,
    result VARCHAR(32) NOT NULL,
    error_code VARCHAR(64) NULL,
    duration_ms BIGINT NOT NULL,
    first_byte_ms BIGINT NULL,
    request_bytes BIGINT NOT NULL,
    response_bytes BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    UNIQUE KEY uk_request_audits_request_id (request_id),
    INDEX idx_request_audits_tenant_created (tenant_id, created_at),
    INDEX idx_request_audits_model_created (model_id, created_at)
);
