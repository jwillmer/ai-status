-- ai-status: Supabase schema for optional cross-device session sync.
--
-- What this does:
--   Creates a single `public.sessions` table that mirrors the local Session
--   struct (one row per markdown file) and locks it down with Row Level
--   Security so each authenticated user can only read/write their own rows.
--   Also creates the indexes the client relies on:
--     - a partial unique index on (user_id, rel_path) WHERE deleted_at IS NULL,
--       so first-sync dedupe by filename is enforced server-side;
--     - a btree index on (user_id, updated_at desc) for the incremental pull.
--
-- Where to paste:
--   Supabase dashboard -> SQL editor -> New query -> paste the whole file ->
--   Run. The script is wrapped in BEGIN/COMMIT, so a partial paste rolls
--   back rather than leaving the table created with RLS disabled.
--
-- Re-running:
--   Safe. Every statement uses `if not exists` (or `create or replace` /
--   `drop policy if exists` for the policies) so applying the script to an
--   already-configured project is a no-op.
--
-- IMPORTANT:
--   Do NOT omit any line. The anon key is public by design; the only thing
--   keeping rows private is RLS + the two policies below. Skipping the
--   `enable row level security` line or either policy will leave every
--   user's data world-readable through the REST API.

begin;

create table if not exists public.sessions (
  id          text primary key,
  user_id     uuid not null default auth.uid() references auth.users(id) on delete cascade,
  title       text not null,
  rel_path    text not null,
  pinned      boolean not null default false,
  archived    boolean not null default false,
  created_at  timestamptz not null,
  updated_at  timestamptz not null default now(),
  deleted_at  timestamptz,
  body        text not null default '',
  body_hash   text not null default ''
);

-- One live row per filename per user. Tombstoned rows (deleted_at not null)
-- are excluded so a user can re-create a previously-deleted filename.
create unique index if not exists sessions_user_relpath_live_uniq
  on public.sessions (user_id, rel_path)
  where deleted_at is null;

-- Supports the incremental pull: "give me everything for this user that has
-- changed since <cursor>", ordered newest-first.
create index if not exists sessions_user_updated_at_idx
  on public.sessions (user_id, updated_at desc);

alter table public.sessions enable row level security;

drop policy if exists "own rows select" on public.sessions;
create policy "own rows select"
  on public.sessions
  for select
  using (user_id = auth.uid());

drop policy if exists "own rows write" on public.sessions;
create policy "own rows write"
  on public.sessions
  for all
  using (user_id = auth.uid())
  with check (user_id = auth.uid());

commit;
