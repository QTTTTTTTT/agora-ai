-- Rollback for 097_admin_feature_flags_and_announcements.

DROP TABLE IF EXISTS announcement_reads;
DROP TABLE IF EXISTS announcements;
DROP TABLE IF EXISTS feature_flags;
