-- Reverse 0028_identity_substrate.

DROP INDEX IF EXISTS devices_principal;
DROP INDEX IF EXISTS devices_sig_key_id;
DROP TABLE IF EXISTS devices;

DROP INDEX IF EXISTS principals_github_user_id;
DROP TABLE IF EXISTS principals;
