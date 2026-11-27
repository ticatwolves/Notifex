-- create "auth_throttles" table
CREATE TABLE "auth_throttles" ("key" character varying NOT NULL, "failures" bigint NOT NULL DEFAULT 0, "last_failure_at" timestamptz NOT NULL, "blocked_until" timestamptz NOT NULL, PRIMARY KEY ("key"));
-- create "email_tokens" table
CREATE TABLE "email_tokens" ("id" uuid NOT NULL, "user_id" uuid NOT NULL, "purpose" character varying NOT NULL, "token_hash" bytea NOT NULL, "created_at" timestamptz NOT NULL, "expires_at" timestamptz NOT NULL, "used_at" timestamptz NULL, PRIMARY KEY ("id"), CONSTRAINT "email_tokens_user_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "emailtoken_expires_at" to table: "email_tokens"
CREATE INDEX "emailtoken_expires_at" ON "email_tokens" ("expires_at");
-- create index "emailtoken_token_hash" to table: "email_tokens"
CREATE UNIQUE INDEX "emailtoken_token_hash" ON "email_tokens" ("token_hash");
-- create index "emailtoken_user_id_purpose" to table: "email_tokens"
CREATE INDEX "emailtoken_user_id_purpose" ON "email_tokens" ("user_id", "purpose");
-- create "sessions" table
CREATE TABLE "sessions" ("id" uuid NOT NULL, "user_id" uuid NOT NULL, "token_hash" bytea NOT NULL, "created_at" timestamptz NOT NULL, "last_seen_at" timestamptz NOT NULL, "absolute_expires_at" timestamptz NOT NULL, "reauthenticated_at" timestamptz NOT NULL, "revoked_at" timestamptz NULL, "user_agent" character varying NOT NULL DEFAULT '', PRIMARY KEY ("id"), CONSTRAINT "sessions_user_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "session_absolute_expires_at" to table: "sessions"
CREATE INDEX "session_absolute_expires_at" ON "sessions" ("absolute_expires_at");
-- create index "session_token_hash" to table: "sessions"
CREATE UNIQUE INDEX "session_token_hash" ON "sessions" ("token_hash");
-- create index "session_user_id_revoked_at" to table: "sessions"
CREATE INDEX "session_user_id_revoked_at" ON "sessions" ("user_id", "revoked_at");
