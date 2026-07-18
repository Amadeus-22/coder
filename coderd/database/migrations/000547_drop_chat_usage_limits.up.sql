DROP TABLE chat_usage_limit_config;

ALTER TABLE users
    DROP CONSTRAINT users_chat_spend_limit_micros_check,
    DROP COLUMN chat_spend_limit_micros;

ALTER TABLE groups
    DROP CONSTRAINT groups_chat_spend_limit_micros_check,
    DROP COLUMN chat_spend_limit_micros;
