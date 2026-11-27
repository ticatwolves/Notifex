-- reverse: create index "useridentity_user_id_provider" to table: "user_identities"
DROP INDEX "useridentity_user_id_provider";
-- reverse: create index "useridentity_provider_subject" to table: "user_identities"
DROP INDEX "useridentity_provider_subject";
-- reverse: create "user_identities" table
DROP TABLE "user_identities";
-- reverse: create index "oauthstate_state_hash" to table: "oauth_states"
DROP INDEX "oauthstate_state_hash";
-- reverse: create index "oauthstate_expires_at" to table: "oauth_states"
DROP INDEX "oauthstate_expires_at";
-- reverse: create "oauth_states" table
DROP TABLE "oauth_states";
