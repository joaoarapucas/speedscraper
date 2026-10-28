-- Leaderboards too large for the API are fetched in slices (subcategory,
-- platform, emulator). split marks them so later fetches go straight to
-- slices; computed_slices counts the slices the API could not serve at all,
-- which were ranked from the stored runs instead.
ALTER TABLE boards ADD COLUMN split INTEGER NOT NULL DEFAULT 0;
ALTER TABLE boards ADD COLUMN computed_slices INTEGER NOT NULL DEFAULT 0;
