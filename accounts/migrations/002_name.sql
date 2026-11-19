-- An account's name, for apps that ask for one (accounts.Config.Names). NULL until the
-- person gives it.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS name text;
