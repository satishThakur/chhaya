# Shipping a change to production

Production is one EC2 box. The desktop is dev. Every change goes desktop first, then the box.
For anything that touches the running stack the recipe is the same; the table below says what
else a given kind of change needs.

## The recipe

**On the desktop (dev bots)**

1. `make test` and `make evals` pass.
2. `make dev-restart`, then the smoke test on the dev bots (`docs/runbook.md`).
3. Merge to `main` and push.

**On the box**

```bash
aws ssm start-session --region ap-south-1 --target i-0741a78bba409c0a9   # from your desktop
sudo -iu ubuntu
cd ~/chhaya

# risky change (policy, Hermes image, anything you could not undo)? back up first:
CHHAYA_BACKUP_S3=s3://chhaya-backup-803874683069-ap-south-1/backups make dev-backup

git pull --ff-only
make dev-doctor        # env files, certs, bot check; changes nothing
make dev-restart       # rebuilds the gateway image, re-renders Hermes' config, restarts both
make dev-status        # gateway refuses a bare client, accepts Hermes' certificate
```

Then in Telegram, on `chhaya_bot`: one reply (not two), a memory question, an approval tap.
A change is not shipped until you have seen that.

`make dev-restart` is the one command for everything below. It costs 20 to 30 seconds of
downtime; nobody is talking to the agent at the times you would do this.

## What else, by kind of change

| You changed | Extra step | Why |
|---|---|---|
| Go code (`cmd/`, `internal/`) | none | `dev-restart` rebuilds the gateway image. |
| `config/policy.yaml` | After restart, the approver bot posts a startup card with the new policy hash. Compare it: `go run ./cmd/chhayactl -policy config/policy.yaml policy explain` | A policy change should never go live unseen. Pending memory cards are re-posted too; that is normal. |
| `hermes/config.yaml` | `make dev-logs hermes`: look for "Connected to Telegram". The `config-migrate ... Read-only file system` error is expected. | `dev.sh` renders this file on every `up`. The model name and API base are not in it; they live in `deploy/.env.hermes` on each machine. |
| `hermes/SOUL.md` | none | Mounted read-only; `dev-restart` makes sure it is re-read. Unverified whether `/new` alone would do. |
| A skill | **Not deployed by git.** See below. | |
| `deploy/.env.hermes` or `.env.gateway` | Edit on that machine only, mode 0600, never commit. `make dev-doctor` before `dev-restart`. | Secrets are per machine. `CHHAYA_ENV` must be `prod` on the box, `dev` on the desktop. |
| Hermes image digest in `deploy/docker-compose.yml` | Read the changelog. `scripts/verify-hermes-image.sh <image@sha256:...>`. Desktop first, backup on the box first. | Hermes is pinned on purpose; never track `main`. |
| `infra/terraform/` | From the desktop: `terraform plan`, read it, then `terraform apply`. | Nothing on the box changes except what the plan says. `user_data` edits never touch a live box. |
| `scripts/box-setup.sh` or `deploy/crontab.box` | On the box: `scripts/box-setup.sh --check --cron`, then `sudo scripts/box-setup.sh --cron`. For a changed crontab: `crontab deploy/crontab.box`. | Keeps the box in step with what the repo says it needs. |
| Memory, audit log, sessions | Never deployed. | Data lives in `~/.chhaya` on each machine and is not in git. |

## Skills

The repo ships no skills. Hermes writes its own, into `~/.chhaya/hermes/skills` on the machine it
ran on, and `skills.write_approval: true` means every one needs your tap. So a skill the agent
wrote on the desktop does not exist on the box, and there is nothing to "deploy". To get one onto
production, ask the agent on `chhaya_bot` to create it there and approve it, or copy the skill
directory into `~/.chhaya/hermes/skills` on the box and run `make dev-restart`. There is no tool
for the copy yet. Skills are data, so the nightly backup covers them.

## If it goes wrong

**Code or config.** On the box: `git log --oneline`, `git checkout <last good commit>`, then
`make dev-restart`. Fix it on the desktop, merge, and later `git checkout main && git pull --ff-only`.

**Data.** Restore never overwrites, so move the bad data aside first:

```bash
make dev-down
mv ~/.chhaya/hermes ~/.chhaya/hermes.bad && mv ~/.chhaya/gateway ~/.chhaya/gateway.bad
aws s3 ls s3://chhaya-backup-803874683069-ap-south-1/backups/         # pick an archive
bin/chhayactl backup pull -dir ~/.chhaya/backups/restore s3://chhaya-backup-803874683069-ap-south-1/backups/<archive>.tar.gz
bin/chhayactl backup restore ~/.chhaya/backups/restore/<archive>.tar.gz
make dev-up
```

If `bin/chhayactl` is missing, build it first: `go build -o bin/chhayactl ./cmd/chhayactl`.
The restore checks every file against the archive's manifest and runs the database and audit-chain
checks before anything is moved into place.

## Never

- Point the desktop at the production bots, or the box at the dev bots. `CHHAYA_ENV` and the bot
  check refuse it; do not edit around them.
- Commit anything from `~/.chhaya` or any `.env` file. The repo is public.
- Run `docker compose` directly on the box; go through `make dev-*` so the environment is set.
