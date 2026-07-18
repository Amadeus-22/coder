ALTER TABLE chat_messages ADD COLUMN total_cost_micros BIGINT;

CREATE INDEX idx_chat_messages_owner_spend
    ON chat_messages (chat_id, created_at)
    WHERE total_cost_micros IS NOT NULL;
