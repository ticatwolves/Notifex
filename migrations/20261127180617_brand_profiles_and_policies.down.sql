-- reverse: create index "policies_tenant_key" to table: "policies"
DROP INDEX "policies_tenant_key";
-- reverse: create index "policies_application_key" to table: "policies"
DROP INDEX "policies_application_key";
-- reverse: create "policies" table
DROP TABLE "policies";
-- reverse: create index "brand_profiles_tenant_key" to table: "brand_profiles"
DROP INDEX "brand_profiles_tenant_key";
-- reverse: create index "brand_profiles_application_key" to table: "brand_profiles"
DROP INDEX "brand_profiles_application_key";
-- reverse: create "brand_profiles" table
DROP TABLE "brand_profiles";
