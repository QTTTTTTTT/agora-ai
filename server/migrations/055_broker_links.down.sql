-- Down migration for 055_broker_links.

DROP TRIGGER IF EXISTS broker_links_touch_updated_at ON broker_links;
DROP FUNCTION IF EXISTS broker_links_touch_updated_at();
DROP TABLE IF EXISTS broker_links;
