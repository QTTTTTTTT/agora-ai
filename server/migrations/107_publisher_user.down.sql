-- Migration: 107_publisher_user (DOWN)
-- Removes the synthetic publisher identity. Order matters:
-- subscriptions before users (FK).

BEGIN;

DELETE FROM subscriptions
 WHERE user_id = '00000000-0000-0000-0000-000000000001'::UUID;

DELETE FROM users
 WHERE id = '00000000-0000-0000-0000-000000000001'::UUID;

COMMIT;
