# Cross-device sync

## What this does

Optional sync of session markdown files and the index (titles, pin/archive state) across the same user's devices, using a Supabase project you own. Resolution is last-write-wins on whole files. The app pulls on start and when you open a session, and pushes on local change. Claude conversation transcripts are not synced: the markdown plan is the handoff, and resuming on another device starts a fresh Claude session that reads the plan.

## What is not synced

- Claude's JSONL transcripts (`~/.claude/projects/...`)
- `data/settings.json` (device-specific paths, window state)
- Terminal scrollback
- Any file outside the configured sessions folder

## Setup (hosted Supabase, ~5 minutes)

1. Create a free Supabase project at https://supabase.com (any region). Note the project URL and the anon (public) API key from Settings -> API.
2. Open SQL Editor -> New query, paste the contents of `supabase/schema.sql` from this repo, click Run. Re-running is safe.
3. In Authentication -> Providers, enable Email. Turn OFF "Confirm email" if you don't want a verification step; otherwise the OTP code email will only arrive after the address is confirmed once.
4. In ai-status, open Settings -> Sync. Paste the project URL and anon key. Enter your email and click "Send code". Type the 6-digit code from the email and click "Verify".
5. First sync runs automatically. Repeat steps 4-5 on every device you want to sync.

## Setup (self-hosted)

Works the same: point the project URL at your self-hosted Supabase. If your GoTrue is on a different origin, fill the "Auth URL override" field under Advanced. The schema and policies are identical.

## How conflicts resolve

Last-write-wins on whole files. If both devices edit the same session offline, the one with the later save wins on next sync and the loser's edit is overwritten. The design assumes a session is being worked on in one place at a time; concurrent multi-device editing is not supported.

## What to do if

- **"I deleted a file but it came back."** Another device had a newer edit; deletes can lose to concurrent edits under last-write-wins. Delete from both devices, or pause the other device first.
- **"Two sessions with the same filename now exist."** First sync deduplicates by filename via the partial unique index. If you genuinely want two, rename one before signing in on the second device.
- **"I want to stop syncing without losing local files."** Toggle Sync off in Settings; local files stay, the Supabase rows stay. Sign out to also clear cached auth tokens.

## Security

The anon key is public by design; security comes from the Row Level Security policies on the `sessions` table. The schema enables RLS in the same transaction that creates the table, so RLS is never on without policies (or vice-versa). Auth tokens are stored in `data/sync-auth.json` (mode 0600 on Unix; on Windows, this relies on user-profile ACLs, so keep the data folder out of shared paths).
