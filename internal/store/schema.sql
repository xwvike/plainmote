CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY,
  github_id TEXT NOT NULL UNIQUE,
  login TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  avatar_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  csrf_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS resources (
  id UUID PRIMARY KEY,
  owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  filename TEXT NOT NULL DEFAULT '',
  content_key TEXT NOT NULL DEFAULT '',
  content_size BIGINT NOT NULL DEFAULT 0 CHECK (content_size >= 0),
  content_type TEXT NOT NULL,
  origin_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS resources_owner_idx ON resources(owner_id);

CREATE TABLE IF NOT EXISTS links (
  id UUID PRIMARY KEY,
  resource_id UUID NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  name TEXT NOT NULL DEFAULT '',
  token_ciphertext BYTEA NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  max_uses INTEGER NOT NULL DEFAULT 0 CHECK (max_uses >= 0),
  used_count INTEGER NOT NULL DEFAULT 0 CHECK (used_count >= 0),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS links_resource_idx ON links(resource_id);

CREATE TABLE IF NOT EXISTS access_logs (
  id UUID PRIMARY KEY,
  resource_id UUID REFERENCES resources(id) ON DELETE SET NULL,
  link_id UUID REFERENCES links(id) ON DELETE SET NULL,
  link_name TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL,
  remote_ip TEXT NOT NULL DEFAULT '',
  remote_addr TEXT NOT NULL DEFAULT '',
  host TEXT NOT NULL DEFAULT '',
  query TEXT NOT NULL DEFAULT '',
  proto TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  referer TEXT NOT NULL DEFAULT '',
  forwarded TEXT NOT NULL DEFAULT '',
  x_forwarded_for TEXT NOT NULL DEFAULT '',
  cf_connecting_ip TEXT NOT NULL DEFAULT '',
  cf_ray TEXT NOT NULL DEFAULT '',
  content_length TEXT NOT NULL DEFAULT '',
  tls BOOLEAN NOT NULL DEFAULT FALSE,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  status SMALLINT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  occurred_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS access_logs_resource_idx ON access_logs(resource_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS access_logs_time_idx ON access_logs(occurred_at DESC);
