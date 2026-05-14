-- v14 -> v15: Add signed prekey rotation timestamp
ALTER TABLE whatsmeow_device ADD COLUMN IF NOT EXISTS signed_pre_key_timestamp BIGINT NOT NULL DEFAULT 0;
