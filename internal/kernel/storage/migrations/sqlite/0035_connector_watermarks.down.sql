-- Down migration 0035: drop the ingestion-source resume-watermark projection.
DROP INDEX IF EXISTS idx_connector_watermarks_connector;
DROP TABLE IF EXISTS connector_watermarks;
