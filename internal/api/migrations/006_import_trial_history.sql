-- Preserve historical Supabase trial timestamps during account migration.
-- This is server-managed history, not an editable entitlement or a new trial grant.
alter table profiles add column trial_ends_at timestamptz;
