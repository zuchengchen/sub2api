ALTER TABLE usage_logs SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);
ALTER TABLE ops_error_logs SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);
ALTER TABLE usage_billing_dedup SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50000);
ALTER TABLE billing_attempt_outbox SET (autovacuum_vacuum_scale_factor = 0.1, autovacuum_vacuum_threshold = 20000);
ALTER TABLE scheduler_outbox SET (autovacuum_vacuum_scale_factor = 0.1, autovacuum_vacuum_threshold = 20000);
ALTER TABLE accounts SET (autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 10000);
