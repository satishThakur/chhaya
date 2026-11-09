# Deployment

How Chhaya goes from this desktop to a single EC2 box in Mumbai, without losing data and
without two machines ever talking through the same Telegram bot. Written 2026-11-08 as the
agreed plan. **Status 2026-11-09: production is live on the box** (steps 1 to 7 done and tested
over Telegram: one reply, memory read, approval tap, `/stop`). Step 8 and the follow-ups are at
the end. Where something is unverified, it says so.

Read `docs/decisions.md` first for why the data layout is the way it is. The short version:
all state is host directories, not Docker volumes, and none of it is in this public repo.

## Decisions (locked)

| Question | Decision |
|---|---|
| Where production runs | One EC2 instance, region **ap-south-1 (Mumbai)**. This desktop is development only. |
| How it is built | **Terraform**, committed to the repo under `infra/terraform/`. |
| Inbound traffic | **None.** Telegram is outbound polling and the gateway listens on localhost. The security group has zero inbound rules. |
| Admin access | **SSM Session Manager**, no SSH, no key pairs. Wanted only for debugging, which this covers. *Assumed from the earlier discussion; confirm before building.* |
| Production bots | The **existing** `chhaya_bot` and `chhaya_approver_bot`. |
| Dev bots | **New**: `chhaya_dev_bot` and `chhaya_dev_approver_bot`, created in BotFather. |
| Production data | Takes **all** current data: Hermes home, gateway data (audit log, kill switch), Personal memory. It is a copy, not a move. |
| Dev data | This desktop keeps its copy. Its digest cron job is disabled so only production sends it. |
| Secrets | Env files on the box, mode 0600, created by hand over SSM, never in the repo. Secrets Manager comes later. |
| Host trust | The host is treated as trusted (`decisions.md`, 2026-11-07). |

## The one thing that must not go wrong: one bot token, one poller

Two processes polling the same bot token fight over `getUpdates` (Telegram answers 409), drop
messages, and can deliver an approval tap to the wrong machine. The fix is two bot pairs, and a
guard that makes the mistake impossible to make quietly.

| Environment | Hermes bot | Approver bot |
|---|---|---|
| prod (EC2) | `chhaya_bot` (existing) | `chhaya_approver_bot` (existing) |
| dev (desktop) | `chhaya_dev_bot` | `chhaya_dev_approver_bot` |

**The guard** (to be built first, with tests):

1. `deploy/.env.hermes` and `deploy/.env.gateway` each declare `CHHAYA_ENV=dev` or `prod`.
2. Preflight (`make dev-doctor` and `dev-up`) calls Telegram `getMe` on each token. `getMe` is
   read-only and does not consume updates.
3. With `CHHAYA_ENV=dev`, both usernames must contain `_dev_`. With `CHHAYA_ENV=prod`, neither
   may. The approver's username must contain `approver` and the Hermes bot's must not, which
   catches swapped tokens.
4. If `getMe` cannot be reached, it refuses to start. Fail closed.

Result: the production token cannot start on the desktop, and a dev token cannot start on the
box. The only way round it is to set `CHHAYA_ENV=prod` on the desktop on purpose.

Tests must cover each refusal: wrong environment for each of the two tokens, swapped tokens,
unreachable Telegram, missing `CHHAYA_ENV`, and an unrecognised value.

## What I checked in the current data (2026-11-08)

- Hermes' own `.env` holds no Telegram or token keys; tokens reach Hermes only through
  `deploy/.env.hermes`. Copying `~/.chhaya` does not carry a token.
- All 35 rows in `delivery_obligations` are `delivered`. Nothing stale fires on first start.
- One scheduled job, "Daily ML/AI Advancements Digest" (`0 3 * * *` UTC), delivers to the
  user's Telegram chat. If dev kept it enabled, dev's bot would send a second digest. It is
  disabled in dev's copy only.
- Not checked: whether Hermes keeps anything else bot-specific in `state.db`
  (`gateway_routing` stores per-session routing entries). Look at it before the cutover.

## Target shape

```
EC2 (ap-south-1), no inbound rules, public IP for outbound only
 ├─ docker compose: gateway + hermes   (same docker-compose.yml as dev)
 ├─ /home/<user>/.chhaya/{hermes,gateway,backups}   on the instance's encrypted EBS volume
 ├─ IAM role: AmazonSSMManagedInstanceCore + read/write on the backup bucket
 └─ certs generated on the box (container-to-container mTLS only)

S3 bucket (private, encrypted, public access blocked): backups and the one-time data handover
Data Lifecycle Manager: daily EBS snapshots, 14 retained
```

A public IP is needed only so the instance can reach Telegram, the model API and the web
without paying for a NAT gateway. The security group has no inbound rules, so nothing can
reach it.

## Terraform (`infra/terraform/`)

Planned resources, in the default VPC to start:

- `aws_instance`: Ubuntu 24.04, **x86_64**, `t3.medium` to start. Chosen because Hermes' browser
  tool ran a real Chromium in the logs. **Unverified:** the real memory use, and whether the
  pinned Hermes image is published for arm64. The pinned digest is a single-platform amd64 manifest
  (checked 2026-11-09), so the box must be x86_64; `t4g` (Graviton) is out until the pin changes.
  On the desktop Hermes used ~300 MiB idle. Start `t3.medium`, measure under real use, resize.
- `aws_security_group`: no ingress; egress 443, 53 and 80. Port 80 was added to the plan: Ubuntu's
  apt mirrors are plain http (packages are signature-checked).
- `aws_iam_role` + instance profile: `AmazonSSMManagedInstanceCore`, plus a policy scoped to
  the backup bucket.
- Root EBS: gp3, encrypted, 30 GB to start.
- `aws_dlm_lifecycle_policy`: daily snapshots, retain 14.
- `aws_s3_bucket` for backups: encrypted, versioned, all public access blocked.
- `user_data`: install Docker, the Compose plugin, git, Go (the bot check runs `chhayactl botcheck` on the host) and the AWS CLI; clone this repo over
  HTTPS (it is public, so no deploy key); do not start the stack. The stack starts only after
  secrets and data are in place.

Built in `infra/terraform/` and applied 2026-11-09 (account 803874683069): instance `i-0741a78bba409c0a9`, bucket `chhaya-backup-803874683069-ap-south-1`. The default VPC is deliberate: the box has no inbound rules and an otherwise empty VPC, and a private subnet would add a NAT gateway for no gain. Move to a custom VPC if the box ever serves inbound traffic or other machines join the VPC. Choices beyond
the list above: IMDSv2 only with hop limit 1, so containers on the box cannot reach the instance
role's credentials; `ignore_changes = [ami, user_data]` on the instance, so a new AMI never replaces
the instance and its data; bucket named `chhaya-backup-<account>-<region>`, noncurrent versions
expire after 30 days; Go and the AWS CLI come from snaps (`go` 1.25/stable and `aws-cli` v2/stable exist in the snap store, checked 2026-11-09).

Terraform state is **not** committed (`*.tfstate` gitignored); it lives in the S3 backend described under open items. Never put a secret in a `.tf` file or a variable default.

## Order of work

Each step leaves the live setup untouched until the cutover.

1. **Guard.** `CHHAYA_ENV`, the `getMe` check, tests first, updated `.env.*.example`, a
   `docs/decisions.md` entry. Touches nothing running.
2. **Dev bots.** Create the two dev bots in BotFather and press Start on the dev approver bot.
   Put dev tokens and `CHHAYA_ENV=dev` in the desktop's env files. Disable the digest job in
   the desktop's `jobs.json`. Back up `~/.chhaya` first. From here the desktop no longer uses the
   production bots.
3. **Prove dev** end to end on the dev bots with the runbook smoke test. The production bots are
   now idle, so nothing is polling them.
4. **Terraform the box.** `terraform plan` reviewed before `apply`. Confirm SSM works
   (`aws ssm start-session`) and that the security group really has no inbound rules.
5. **Data handover.** On the desktop: stop the stack, take a verified backup, write a tarball of
   `~/.chhaya/{hermes,gateway}` to the backup bucket. On the box: pull it, extract, verify
   (below). The desktop keeps its copy. This is the first real restore, so the backup/restore
   script gets written and tested here, and the eval work reuses it.
6. **Box secrets.** Create `deploy/.env.hermes` and `deploy/.env.gateway` on the box over SSM,
   mode 0600, with the **production** tokens and `CHHAYA_ENV=prod`. Run `make dev-certs`.
7. **Cutover.** `make dev-up` on the box. Send a message to `chhaya_bot`; check a memory read, an
   approval tap and `/stop` on `chhaya_approver_bot`. Only now is production live.
8. **After.** Delete the handover object from the bucket. Confirm the first EBS snapshot ran.

## Handover done (2026-11-09)

A shell script (since replaced by `chhayactl backup`, below) took a verified tarball of
`~/.chhaya/{hermes,gateway}` with the stack down, tested a restore into a scratch directory
(including refusing to overwrite and catching a tampered file), uploaded it to
`s3://chhaya-backup-803874683069-ap-south-1/handover/`, and restored it on the box. Counts
matched the manifest: state.db ok, 400 messages, 13 sessions, 13 gateway_routing rows, 57 audit
records with the chain intact from genesis on the box, same memory git head. Lock files and
sockets are excluded on purpose (a pid from another machine means nothing). `gateway_routing`
entries are per-session DMs, with no bot token in them. The handover object stays in the bucket
until the cutover succeeds (step 8).

## Verification before declaring production live

- `state.db` passes `PRAGMA integrity_check` and has at least the 130 messages and 2 sessions
  recorded on 2026-11-08 (it will have more by then; compare against the desktop's count).
- `chhayactl audit verify` reports the chain intact from genesis, with at least the same record
  count as the desktop.
- `git -C ~/.chhaya/gateway/memory log` matches the desktop's, and `memory.list` returns the
  same documents.
- The digest job is present and enabled on the box, and disabled on the desktop.
- `make dev-doctor` on the box passes, including the bot check, and `make dev-status` shows the
  gateway refusing an uncertificated client and accepting Hermes'.
- A test message to `chhaya_bot` gets one reply, not two.

## Rollback

The desktop keeps a full copy and has already moved to the dev bots, so rollback is: stop the
box (`make dev-down`), set the desktop's env files back to the production tokens and
`CHHAYA_ENV=prod`, and start it. Conversations made on the box after the cutover would be on the
box only; take a backup from the box first if there are any worth keeping.

## Risks and open items

- **SSM access** was assumed, not explicitly confirmed. Confirm before step 4.
- **Instance size and CPU architecture** are unverified (see Terraform above).
- **Region:** ap-south-1 confirmed. No existing VPC or account details have been given, so the
  plan uses the default VPC. Say if there is a reason not to.
- **`gateway_routing` in `state.db`** may hold bot- or machine-specific entries. Inspect before
  the cutover.
- **Terraform state** is local at first, so losing the laptop loses the state, not the box.
- **Cost** has not been estimated. A `t3.medium`, 30 GB gp3, snapshots and a small bucket are
  all low and none is a reason to change the plan, but nothing here has been priced.
- **The guard depends on bot naming.** A bot renamed in BotFather outside the convention will
  be refused until the convention or the check is updated.
- **Secrets on the box** are plain files. Fine under the trusted-host assumption; Secrets
  Manager is the planned upgrade.

## Nightly backup

`chhayactl backup create | verify | restore | push | pull | status` (Go, `internal/backup`) runs
from the host, where both `~/.chhaya/hermes` and `~/.chhaya/gateway` are visible. `scripts/dev.sh
backup` (`make dev-backup`) wraps it: stop the stack, take a cold copy, restart, then upload to
`s3://<bucket>/backups/`. Cron on the box runs it at **02:00 IST (20:30 UTC)** from
`deploy/crontab.box`. S3 keeps **30 days** (lifecycle rule in `infra/terraform/s3.tf`); the box
keeps the newest three locally.

Each archive holds `MANIFEST.json` (every file's size, mode and SHA-256) and has a `.sha256`
sidecar. Restore needs both, never overwrites, extracts to a temporary directory, checks the
manifest, runs `PRAGMA integrity_check` on every database and verifies the audit chain, and only
then moves the data into place. It refuses symlinks, absolute or `..` paths and anything outside
`hermes/` and `gateway/`. Create refuses a database that fails its check or a broken audit chain.
The box needs the `sqlite3` CLI; without it the check fails closed.

`make dev-status` on prod warns if the last successful upload is older than 36 hours.

## After the cutover (open items)

- **Step 8:** the handover tarball is still in the bucket under `handover/`. Delete it once
  you are sure the box needs no rebuild from it.
- **First EBS snapshot** has not been confirmed. DLM runs daily at 21:00 UTC; check
  `aws ec2 describe-snapshots --owner-ids self` after that.
- **Terraform state** moved to S3 on 2026-11-09: `s3://chhaya-backup-803874683069-ap-south-1/terraform/chhaya-box.tfstate` (same bucket as the backups, encrypted, versioned, S3-native locking). It holds no secrets (scanned). The box's role can read and write that bucket, so it could touch its own state; accepted under the trusted-host assumption, and a separate bucket is the fix if that changes. A pre-migration copy is in `~/.chhaya/backups/terraform.tfstate.pre-s3-backend`.
- **Digest job** is enabled on both the desktop copy and the box, so both send it. Disable it
  in the desktop's `~/.chhaya/hermes/cron/jobs.json` when that stops being wanted.
- **Admin access keys** for `chhaya-admin` are in `~/.aws/credentials`. Rotate or delete them
  when no longer building.
- **Nightly backup to S3** is live since 2026-11-09: crontab installed on the box (02:00 IST),
  30-day S3 expiry applied, first run uploaded and restored into a scratch directory. The first
  scheduled run is 2026-11-09 20:30 UTC; check `make dev-status` on the box or
  `tail ~/.chhaya/backups/backup.log` after it.
- The desktop's old Docker volumes `deploy_hermes-data` and `deploy_gateway-data` still exist.

## Not in scope

New features, the evaluation tooling, Secrets Manager, and a
multi-instance or HA setup. Eval and measurement tooling comes after deployment is stable.
