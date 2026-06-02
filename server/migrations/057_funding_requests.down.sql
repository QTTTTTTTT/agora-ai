-- Down migration for 057_funding_requests.

DROP TRIGGER IF EXISTS funding_requests_touch_updated_at ON funding_requests;
DROP FUNCTION IF EXISTS funding_requests_touch_updated_at();
DROP TABLE IF EXISTS funding_requests;
