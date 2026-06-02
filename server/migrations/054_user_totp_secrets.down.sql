-- Down migration for 054_user_totp_secrets.
--
-- Drops the table and the helper trigger function. NB: dropping the
-- function is safe because no other migration uses it today.
DROP TRIGGER IF EXISTS user_totp_touch_updated_at ON user_totp_secrets;
DROP TABLE IF EXISTS user_totp_secrets;
DROP FUNCTION IF EXISTS user_totp_touch_updated_at();
