-- Children first: both reference users.
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;

-- citext is left installed. Dropping an extension is not reversible in any
-- useful sense if something else has come to depend on it, and an unused
-- extension costs nothing.
