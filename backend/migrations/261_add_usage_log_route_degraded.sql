-- route_degraded is tri-state:
--   NULL  = Tibo route selection did not apply (non Cookie-WS accounts, other
--           platforms, or rows written before this migration).
--   FALSE = Tibo routing applied and a route with a healthy/unknown verdict
--           served the request.
--   TRUE  = Tibo routing applied, every route for the account was
--           degraded/unavailable, and the request was served on the plain HTTP
--           fallback ("降智兜底").
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_degraded BOOLEAN;
