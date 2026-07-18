CREATE TABLE chat_usage_limit_config (
    id BIGSERIAL PRIMARY KEY,
    singleton BOOLEAN NOT NULL DEFAULT TRUE CHECK (singleton),
    UNIQUE (singleton),
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    default_limit_micros BIGINT NOT NULL DEFAULT 0
        CHECK (default_limit_micros >= 0),
    period TEXT NOT NULL DEFAULT 'month'
        CHECK (period IN ('day', 'week', 'month')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO chat_usage_limit_config (singleton) VALUES (TRUE);

ALTER TABLE users ADD COLUMN chat_spend_limit_micros BIGINT DEFAULT NULL
    CHECK (chat_spend_limit_micros IS NULL OR chat_spend_limit_micros > 0);

ALTER TABLE groups ADD COLUMN chat_spend_limit_micros BIGINT DEFAULT NULL
    CHECK (chat_spend_limit_micros IS NULL OR chat_spend_limit_micros > 0);
