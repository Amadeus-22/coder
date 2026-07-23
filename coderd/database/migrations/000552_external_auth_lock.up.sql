ALTER TABLE external_auth_links ADD COLUMN IF NOT EXISTS refresh_lease_expires_at timestamp WITH time zone DEFAULT NULL;
COMMENT ON COLUMN external_auth_links.refresh_lease_expires_at IS 'Use to hold a lease on the row to prevent concurrent refreshes.';
