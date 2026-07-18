DROP INDEX IF EXISTS idx_chat_messages_owner_spend;

ALTER TABLE chat_messages DROP COLUMN total_cost_micros;
