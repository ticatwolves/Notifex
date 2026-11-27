-- create "oauth_states" table
CREATE TABLE "oauth_states" ("id" uuid NOT NULL, "state_hash" bytea NOT NULL, "provider" character varying NOT NULL, "redirect_uri" character varying NOT NULL, "code_verifier" character varying NOT NULL, "nonce" character varying NOT NULL, "link_user_id" uuid NULL, "created_at" timestamptz NOT NULL, "expires_at" timestamptz NOT NULL, "used_at" timestamptz NULL, PRIMARY KEY ("id"), CONSTRAINT "oauth_states_link_user_fk" FOREIGN KEY ("link_user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "oauthstate_expires_at" to table: "oauth_states"
CREATE INDEX "oauthstate_expires_at" ON "oauth_states" ("expires_at");
-- create index "oauthstate_state_hash" to table: "oauth_states"
CREATE UNIQUE INDEX "oauthstate_state_hash" ON "oauth_states" ("state_hash");
-- create "user_identities" table
CREATE TABLE "user_identities" ("id" uuid NOT NULL, "user_id" uuid NOT NULL, "provider" character varying NOT NULL, "subject" character varying NOT NULL, "email" character varying NOT NULL DEFAULT '', "created_at" timestamptz NOT NULL, "last_used_at" timestamptz NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "user_identities_user_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "useridentity_provider_subject" to table: "user_identities"
CREATE UNIQUE INDEX "useridentity_provider_subject" ON "user_identities" ("provider", "subject");
-- create index "useridentity_user_id_provider" to table: "user_identities"
CREATE UNIQUE INDEX "useridentity_user_id_provider" ON "user_identities" ("user_id", "provider");
