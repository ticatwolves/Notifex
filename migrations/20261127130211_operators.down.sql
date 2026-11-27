-- reverse: create index "operatorsession_token_hash" to table: "operator_sessions"
DROP INDEX "operatorsession_token_hash";
-- reverse: create index "operatorsession_operator_id_revoked_at" to table: "operator_sessions"
DROP INDEX "operatorsession_operator_id_revoked_at";
-- reverse: create index "operatorsession_absolute_expires_at" to table: "operator_sessions"
DROP INDEX "operatorsession_absolute_expires_at";
-- reverse: create "operator_sessions" table
DROP TABLE "operator_sessions";
-- reverse: create index "operators_email_key" to table: "operators"
DROP INDEX "operators_email_key";
-- reverse: create "operators" table
DROP TABLE "operators";
