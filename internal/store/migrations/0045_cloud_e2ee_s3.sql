-- Zero-knowledge client-side E2EE for third-party cloud providers and native S3/R2/MinIO settings.
ALTER TABLE cloud_config ADD COLUMN e2ee_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cloud_config ADD COLUMN e2ee_passphrase TEXT NOT NULL DEFAULT '';
ALTER TABLE cloud_config ADD COLUMN s3_region TEXT NOT NULL DEFAULT '';
ALTER TABLE cloud_config ADD COLUMN s3_bucket TEXT NOT NULL DEFAULT '';
ALTER TABLE cloud_config ADD COLUMN s3_prefix TEXT NOT NULL DEFAULT '';
ALTER TABLE cloud_config ADD COLUMN s3_path_style INTEGER NOT NULL DEFAULT 0;
