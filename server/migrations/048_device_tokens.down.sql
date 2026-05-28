-- 048_device_tokens.down.sql
DROP INDEX IF EXISTS device_tokens_user_idx;
DROP INDEX IF EXISTS device_tokens_user_token_uniq;
DROP TABLE IF EXISTS device_tokens;
