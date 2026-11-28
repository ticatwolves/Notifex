-- reverse: create index "channelcredential_tenant_id_id" to table: "channel_credentials"
DROP INDEX "channelcredential_tenant_id_id";
-- reverse: create index "channel_credentials_tenant_scope_key" to table: "channel_credentials"
DROP INDEX "channel_credentials_tenant_scope_key";
-- reverse: create index "channel_credentials_env_scope_key" to table: "channel_credentials"
DROP INDEX "channel_credentials_env_scope_key";
-- reverse: create index "channel_credentials_app_scope_key" to table: "channel_credentials"
DROP INDEX "channel_credentials_app_scope_key";
-- reverse: create "channel_credentials" table
DROP TABLE "channel_credentials";
