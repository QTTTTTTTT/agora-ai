-- F31 down for F27 (028_admin_dual_control.sql).
-- Drops the two-person approval queue and behaviour-diff log.
-- Application code MUST be torn down before this runs — any handler
-- still calling DualControlService.Submit will start returning 500
-- once the table is gone.

DROP INDEX IF EXISTS admin_change_log_action_idx;
DROP INDEX IF EXISTS admin_change_log_target_idx;
DROP INDEX IF EXISTS admin_change_log_actor_idx;
DROP TABLE IF EXISTS admin_change_log;

DROP INDEX IF EXISTS admin_requests_requester_idx;
DROP INDEX IF EXISTS admin_requests_status_idx;
DROP TABLE IF EXISTS admin_requests;
