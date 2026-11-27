-- create "operators" table
CREATE TABLE "operators" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "email" character varying NOT NULL, "name" character varying NOT NULL DEFAULT '', "password_hash" character varying NOT NULL, "totp_wrapped_key" bytea NOT NULL, "totp_key_id" character varying NOT NULL, "totp_ciphertext" bytea NOT NULL, "totp_last_step" bigint NOT NULL DEFAULT 0, "status" character varying NOT NULL DEFAULT 'active', PRIMARY KEY ("id"));
-- create index "operators_email_key" to table: "operators"
CREATE UNIQUE INDEX "operators_email_key" ON "operators" ("email");
-- create "operator_sessions" table
CREATE TABLE "operator_sessions" ("id" uuid NOT NULL, "operator_id" uuid NOT NULL, "token_hash" bytea NOT NULL, "created_at" timestamptz NOT NULL, "last_seen_at" timestamptz NOT NULL, "absolute_expires_at" timestamptz NOT NULL, "revoked_at" timestamptz NULL, "user_agent" character varying NOT NULL DEFAULT '', PRIMARY KEY ("id"), CONSTRAINT "operator_sessions_operator_fk" FOREIGN KEY ("operator_id") REFERENCES "operators" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "operatorsession_absolute_expires_at" to table: "operator_sessions"
CREATE INDEX "operatorsession_absolute_expires_at" ON "operator_sessions" ("absolute_expires_at");
-- create index "operatorsession_operator_id_revoked_at" to table: "operator_sessions"
CREATE INDEX "operatorsession_operator_id_revoked_at" ON "operator_sessions" ("operator_id", "revoked_at");
-- create index "operatorsession_token_hash" to table: "operator_sessions"
CREATE UNIQUE INDEX "operatorsession_token_hash" ON "operator_sessions" ("token_hash");
