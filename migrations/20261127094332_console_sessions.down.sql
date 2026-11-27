-- reverse: create index "session_user_id_revoked_at" to table: "sessions"
DROP INDEX "session_user_id_revoked_at";
-- reverse: create index "session_token_hash" to table: "sessions"
DROP INDEX "session_token_hash";
-- reverse: create index "session_absolute_expires_at" to table: "sessions"
DROP INDEX "session_absolute_expires_at";
-- reverse: create "sessions" table
DROP TABLE "sessions";
-- reverse: create index "emailtoken_user_id_purpose" to table: "email_tokens"
DROP INDEX "emailtoken_user_id_purpose";
-- reverse: create index "emailtoken_token_hash" to table: "email_tokens"
DROP INDEX "emailtoken_token_hash";
-- reverse: create index "emailtoken_expires_at" to table: "email_tokens"
DROP INDEX "emailtoken_expires_at";
-- reverse: create "email_tokens" table
DROP TABLE "email_tokens";
-- reverse: create "auth_throttles" table
DROP TABLE "auth_throttles";
