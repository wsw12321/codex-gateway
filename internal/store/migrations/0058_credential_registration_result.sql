-- Preserve the outcome from the first ownership claim through commit and replay.
ALTER TABLE egress_registrations ADD COLUMN already_present boolean NOT NULL DEFAULT false;
