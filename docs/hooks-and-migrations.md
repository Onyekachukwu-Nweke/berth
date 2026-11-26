# Release hooks and migrations

A hook is a command you define that the deploy runs at a named point. Hooks exist
so you do not have to hand-edit generated YAML to run a migration, warm a cache or
flush a queue.

## Where hooks run in a deploy

```
pre_build → build (on the runner) → post_build → migration
    → [gate: unapproved migrations stop here] → pre_deploy
    → activate → post_deploy → health_check
```

`rollback` hooks run only during a rollback.

**`migration` runs before the swap, deliberately.** That is the "expand" half of
expand/contract: an additive schema change goes in while the old code is still
serving, so both the old and the new code work against it. Running it after the
swap would mean new code briefly meeting an old schema.

## Rolling back code does not roll back your database

This is the most important thing on this page.

A rollback re-points your containers at a previous image. It does **not** undo a
migration that has already run. An applied migration usually cannot be undone by
running old code again — a dropped column is gone, a changed type has already
converted the data.

So write migrations that are safe against **both** the old and the new code:

1. **Expand.** Add the new column, table or index. Deploy. Old code ignores it;
   new code uses it.
2. **Migrate the data**, if needed, in a way both versions tolerate.
3. **Contract**, in a *later* release, once you are confident you will not roll
   back past it: drop the old column.

If you roll back past a migration that only expanded, nothing breaks. That is the
whole point of doing it in that order.

The `rollback` hook is for **compensating** actions — clearing a cache, flipping a
feature flag back. It is not a down-migration, and berth will not pretend it is.

## The confirmation gate

A hook marked "requires confirmation" is **never run by a `git push`**. A push has
no human at the keyboard, so there is nobody to confirm anything.

When a push reaches a gated migration, the run:

- builds and records the new release,
- **stops before activating it**,
- leaves the previous release serving,
- and marks the deployment as awaiting confirmation.

You then review the exact command in berth and approve it. Approving runs the
migration and continues the deploy that halted.

Skipping the migration and carrying on would activate new code against a schema
that was never migrated — exactly what the ordering above exists to prevent. So
the gate stops the deploy rather than warning about it.

**Migrations in `production` are always gated.** You cannot turn that off, because
"production requires confirmation" means nothing if the actor is a push.

### What an approval covers

An approval names one release and the exact set of hook commands you reviewed. If
a newer deploy supersedes that release, or you edit the command afterwards, the
approval no longer applies and you are asked again. You approved what you read.

### What the gate is, and is not

The approval marker is deliberately absent from the generated workflow, so the
Actions runner cannot set it — not even via a repository variable of that name.

That is protection against **accidental** execution. It is **not** an
authorization boundary: anyone who can push to your repository can change the
committed deploy script, which runs as your deploy user. Hooks are your commands
running on your server, with the same trust model as the terminal page.

## Where a hook runs

| Target | Behaviour |
|---|---|
| `container` (default) | A fresh container from the service's image, with the app's environment |
| `host` | The deploy directory on the server |

Container hooks use `docker compose run --rm`, not `exec`. `exec` needs the
container already running, so it cannot express a migration that must precede the
swap — and it would run inside the container serving traffic, where a migration
that wedges or exhausts memory takes production with it.

`run --rm` is **not isolated from production**: the hook shares the project's
configured networks and volumes. That is precisely how a migration reaches your
database, and worth knowing before you write anything destructive.

Dependencies are **not** started by default (`--no-deps`). A hook that quietly
starts half your stack is a surprise, and a migration usually only needs the
database, which is already up. Turn it on per hook if you need it.

A `container` hook at `pre_build` is rejected: there is no image yet. Use `host`,
or a later phase.

## Timeouts and unknown outcomes

Every hook has a timeout (15 minutes by default). When it fires, berth stops
the hook's **container**, not just its own connection — abandoning a migration
that is still writing to your database is the worst of both outcomes.

If a migration's outcome cannot be determined — the connection dropped, or it was
stopped mid-flight — the deployment is marked **blocked**. Nothing else runs
against it, including deploys and rollbacks, until you confirm whether the
migration applied. Guessing is how a schema and its code silently diverge.

## Examples

```
php artisan migrate --force
python manage.py migrate
rails db:migrate
npm run db:migrate
go run ./cmd/migrate
```
