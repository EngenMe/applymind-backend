-- Deleting the demo user cascades to its tokens and any data seeded under it.
DELETE FROM users WHERE email = 'demo@applymind.faroukhasnaoui.tech';

ALTER TABLE api_tokens DROP COLUMN IF EXISTS is_read_only;