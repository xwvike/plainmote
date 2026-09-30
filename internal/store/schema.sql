CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY,
  github_id TEXT NOT NULL UNIQUE,
  login TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  avatar_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  -- Quick shares made by this account are encrypted in the browser. Off
  -- unless the account turns it on in its settings.
  e2ee BOOLEAN NOT NULL DEFAULT FALSE
);
ALTER TABLE users ADD COLUMN IF NOT EXISTS e2ee BOOLEAN NOT NULL DEFAULT FALSE;
-- An account the operator has suspended: it cannot sign in and its links
-- deliver nothing, but nothing it holds is deleted. The reason is shown to
-- its owner at sign-in.
ALTER TABLE users ADD COLUMN IF NOT EXISTS suspended_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS suspended_reason TEXT NOT NULL DEFAULT '';
-- When the account last signed in, for the operator's overview.
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_signed_in_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS plans (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  max_resources INTEGER NOT NULL CHECK (max_resources >= 0),
  max_storage BIGINT NOT NULL CHECK (max_storage >= 0),
  is_default BOOLEAN NOT NULL DEFAULT FALSE,
  valid_from TIMESTAMPTZ,
  valid_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS plans_default_idx ON plans ((1)) WHERE is_default;

-- An account is limited by storage. The resource count is a fuse against a
-- flood of tiny files, set far above what the storage allows in practice.
INSERT INTO plans (id, name, max_resources, max_storage, is_default, created_at)
VALUES (gen_random_uuid(), 'default', 1000, 104857600, TRUE, now())
ON CONFLICT (name) DO NOTHING;
-- The default plan used to be 20 resources and 10 MiB. Raised only where it
-- still reads exactly that, so a plan an operator has changed is left alone.
UPDATE plans SET max_resources = 1000, max_storage = 104857600, updated_at = now()
WHERE name = 'default' AND max_resources = 20 AND max_storage = 10485760;

CREATE TABLE IF NOT EXISTS user_plans (
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  plan_id UUID NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  granted_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ,
  PRIMARY KEY (user_id, plan_id)
);
CREATE INDEX IF NOT EXISTS user_plans_plan_idx ON user_plans(plan_id);

-- The account every anonymous paste belongs to. A real row rather than a null
-- owner, so owner_id stays NOT NULL everywhere and the quota, the access log
-- and the delete path all keep working unchanged. Nobody can sign in as it:
-- github_id here is not numeric, and every real account is created from a
-- numeric GitHub id.
INSERT INTO users (id, github_id, login, name, avatar_url, created_at, updated_at)
VALUES ('00000000-0000-0000-0000-000000000001', 'anonymous', 'anonymous', '匿名', '', now(), now())
ON CONFLICT (github_id) DO NOTHING;

-- A fuse, not a quota. Anonymous writing is held down by a short link lifetime
-- and by rate limiting; this only stops the process if both of those have
-- already failed, and it is deliberately far above normal use.
INSERT INTO plans (id, name, max_resources, max_storage, is_default, created_at)
VALUES ('00000000-0000-0000-0000-000000000002', 'anonymous', 100000, 21474836480, FALSE, now())
ON CONFLICT (name) DO NOTHING;
-- Raised from 10 GiB when quick shares could last a month instead of half an
-- hour. Only from the old value, so an operator's own setting stays.
UPDATE plans SET max_storage = 21474836480, updated_at = now()
WHERE name = 'anonymous' AND max_storage = 10737418240;

INSERT INTO user_plans (user_id, plan_id, granted_at)
SELECT '00000000-0000-0000-0000-000000000001', id, now() FROM plans WHERE name = 'anonymous'
ON CONFLICT (user_id, plan_id) DO NOTHING;

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
  content_encoding TEXT NOT NULL DEFAULT '',
  origin_url TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS resources_owner_idx ON resources(owner_id);
-- The content a resource holds now is its current version. version counts the
-- saves that changed the content; version_at is when this one was saved, which
-- updated_at is not - that moves with a rename too. restored_from names the
-- version this content was brought back from, if it was. content_sha256 lets a
-- save that changes nothing be told apart without reading the stored object;
-- rows from before it have none, and their next save is simply a new version.
ALTER TABLE resources ADD COLUMN IF NOT EXISTS version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS version_at TIMESTAMPTZ;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS restored_from INTEGER;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS content_sha256 TEXT NOT NULL DEFAULT '';
UPDATE resources SET version_at = updated_at WHERE version_at IS NULL;
-- A resource the operator has taken down: its links deliver nothing and no
-- new ones can be made; its owner sees the reason and may delete it.
ALTER TABLE resources ADD COLUMN IF NOT EXISTS taken_down_at TIMESTAMPTZ;
ALTER TABLE resources ADD COLUMN IF NOT EXISTS takedown_reason TEXT NOT NULL DEFAULT '';

-- What a resource held before, one row per replaced version. Kept whole rather
-- than as changes against the next one, so any version can be read, compared
-- or dropped on its own. replaced_at is when it stopped being current, and is
-- what the retention period counts from: the version replaced a minute ago is
-- the one most worth keeping, however long ago it was first saved.
CREATE TABLE IF NOT EXISTS resource_versions (
  resource_id UUID NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  version INTEGER NOT NULL,
  content_key TEXT NOT NULL,
  content_size BIGINT NOT NULL CHECK (content_size >= 0),
  content_type TEXT NOT NULL,
  content_encoding TEXT NOT NULL DEFAULT '',
  content_sha256 TEXT NOT NULL DEFAULT '',
  restored_from INTEGER,
  saved_at TIMESTAMPTZ NOT NULL,
  replaced_at TIMESTAMPTZ NOT NULL,
  -- The filename the resource had while this was its content. Only a
  -- restore reads it, and only when the current name would misname what
  -- comes back: an image restored over a video.
  filename TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (resource_id, version)
);
ALTER TABLE resource_versions ADD COLUMN IF NOT EXISTS filename TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS resource_versions_replaced_idx ON resource_versions(replaced_at);

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
  created_at TIMESTAMPTZ NOT NULL,
  -- When the current terms began: set when they are changed, NULL until then,
  -- when they began with the link. Expiry is counted from here.
  terms_at TIMESTAMPTZ
);
ALTER TABLE links ADD COLUMN IF NOT EXISTS terms_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS links_resource_idx ON links(resource_id);

-- A signed-in creator may explicitly keep a quick share. The resource remains
-- anonymous until then, and this row binds that one action to the account that
-- created it; possessing the public link alone is not enough to claim it.
CREATE TABLE IF NOT EXISTS paste_claims (
  resource_id UUID PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS paste_claims_user_idx ON paste_claims(user_id);

-- Names used to be stored with a Chinese placeholder when none was given; the
-- placeholder is now supplied at render time in the reader's language. This
-- clears the ones already written and finds nothing to do after the first run.
UPDATE resources SET name = '' WHERE name IN ('未命名资源', '匿名内容') AND filename = '';

-- One row per data export, kept only as long as the rate window needs it.
CREATE TABLE IF NOT EXISTS account_exports (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS account_exports_user_idx ON account_exports(user_id, created_at);

CREATE TABLE IF NOT EXISTS access_logs (
  id UUID PRIMARY KEY,
  -- The log outlives what it describes. owner_id says who may read the row and
  -- resource_name/resource_file say what was reached, so deleting a resource
  -- does not take its history with it.
  owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  resource_id UUID REFERENCES resources(id) ON DELETE SET NULL,
  resource_name TEXT NOT NULL DEFAULT '',
  resource_file TEXT NOT NULL DEFAULT '',
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
  -- A refusal repeated by the same caller folds into one row rather than
  -- dropping: hits counts the attempts and first_at holds when the run began,
  -- so occurred_at stays the most recent one.
  hits INTEGER NOT NULL DEFAULT 1 CHECK (hits > 0),
  first_at TIMESTAMPTZ NOT NULL,
  occurred_at TIMESTAMPTZ NOT NULL,
  -- Which version of the content was delivered. Only a delivery of stored
  -- content has one: a refusal delivered nothing, and a remote resource is
  -- whatever its origin returned.
  resource_version INTEGER
);
ALTER TABLE access_logs ADD COLUMN IF NOT EXISTS resource_version INTEGER;
-- Which version each link last delivered. Partial, so rows from before
-- versions were recorded, and refusals, cost it nothing.
CREATE INDEX IF NOT EXISTS access_logs_delivered_idx ON access_logs(link_id, occurred_at DESC) WHERE resource_version IS NOT NULL;
CREATE INDEX IF NOT EXISTS access_logs_fold_idx ON access_logs(link_id, outcome, remote_ip, occurred_at DESC) WHERE link_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS access_logs_owner_idx ON access_logs(owner_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS access_logs_resource_idx ON access_logs(resource_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS access_logs_time_idx ON access_logs(occurred_at DESC);

-- The access log copied the same placeholders; see the resources update above.
UPDATE access_logs SET link_name = '' WHERE link_name = '未命名分享';
UPDATE access_logs SET resource_name = '' WHERE resource_name IN ('未命名资源', '匿名内容') AND resource_file = '';

-- Every change made through the admin interface, append only. Targets are
-- plain text rather than references, so the record outlives what it names.
CREATE TABLE IF NOT EXISTS admin_audit (
  id UUID PRIMARY KEY,
  at TIMESTAMPTZ NOT NULL,
  key_id TEXT NOT NULL,
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  remote_ip TEXT NOT NULL DEFAULT ''
);
-- target_label is what the target was called when the change was made, so
-- the record still names it after it is gone; detail carries structured
-- facts about the change as JSON, apart from the operator's own reason.
ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS target_label TEXT NOT NULL DEFAULT '';
ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS detail JSONB;
CREATE INDEX IF NOT EXISTS admin_audit_at_idx ON admin_audit(at DESC);
CREATE INDEX IF NOT EXISTS admin_audit_target_idx ON admin_audit(target_id, at DESC);
