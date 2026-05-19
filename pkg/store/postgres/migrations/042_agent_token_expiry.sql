-- Bound the blast radius of a leaked agent bearer token. NULL = no expiry
-- (preserves long-lived daemon-agent tokens). Non-NULL = short-lived,
-- enforced by RequireAgent middleware.
ALTER TABLE agents ADD COLUMN token_expires_at TIMESTAMPTZ;
