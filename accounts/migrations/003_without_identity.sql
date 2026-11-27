-- An account without an identity, such as one a marketplace sale opens for an AI agent,
-- has no primary address. Nobody signs in to it: a new identity joins an account only
-- through its primary address, which NULL never matches.
ALTER TABLE accounts ALTER COLUMN primary_email DROP NOT NULL;
