# How environment variables reach the container

This walks through how an environment variable you enter in Berth ends up inside
the running application container at deploy time.

**The one-line summary:** only the variable **names** are baked into the committed
GitHub Actions workflow and `deploy.sh`. The **values** only ever travel as GitHub
encrypted secrets, then as ephemeral shell/file state on the runner and your server.
The committed artifacts never contain a value.

## The single source of the names

Everything keys off one function, `Config.envNames()` in
[`backend/internal/workflow/workflow.go`](../backend/internal/workflow/workflow.go) —
it returns the operator's env-var **keys** (blank keys dropped). It is populated from
your UI input in `buildConfig` in
[`backend/internal/usecase/deploy.go`](../backend/internal/usecase/deploy.go), which
copies `in.EnvVars` into the workflow config. Only `.Key` is used in the generated
files; `.Value` never appears in them — it is used exactly once, to set the GitHub
secret.

## Hop by hop, for a variable `VAR=val`

### 1. You type `VAR=val` → it becomes a GitHub environment secret named `VAR`

`deploySecrets` (in `deploy.go`) builds the full secret set: the four managed
connection secrets plus one per operator variable.

```go
func deploySecrets(server Server, sshKey string, in DeployInput) []NamedSecret {
	secrets := []NamedSecret{
		{Name: "SERVER_HOST", Value: server.IP},
		{Name: "SERVER_USER", Value: BootstrapUser},
		{Name: "SERVER_SSH_KEY", Value: sshKey},
		{Name: "DEPLOY_DIR", Value: strings.TrimSpace(in.DeployDir)},
	}
	for _, v := range in.EnvVars {
		if k := strings.TrimSpace(v.Key); k != "" {
			secrets = append(secrets, NamedSecret{Name: k, Value: v.Value})
		}
	}
	return secrets
}
```

`SetEnvSecrets` uploads these to the GitHub deployment environment, each value
encrypted with the environment's public key. **This is the only moment the value
`val` leaves Berth.** Everything downstream references secrets *by name only*,
because GitHub Actions secret values are write-only.

### 2. The workflow bakes in the names and exposes them on the deploy step

`Workflow()` (in `workflow.go`) builds an `env:` block — one line per name,
`VAR: ${{ secrets.VAR }}` — appends `DEPLOY_DIR`, and passes the operator names as a
single `MOUNTABO_ENV_KEYS` list so `deploy.sh` knows which to write without the names
being baked into the script. The rendered YAML for the deploy step looks like:

```yaml
- name: Deploy on the server
  uses: appleboy/ssh-action@v1.1.0
  env:
    VAR: ${{ secrets.VAR }}
    DEPLOY_DIR: ${{ secrets.DEPLOY_DIR }}
    MOUNTABO_ENV_KEYS: "VAR"
  with:
    host: ${{ secrets.SERVER_HOST }}
    username: ${{ secrets.SERVER_USER }}
    key: ${{ secrets.SERVER_SSH_KEY }}
    envs: VAR,DEPLOY_DIR,MOUNTABO_ENV_KEYS
    script: |
      /tmp/<app>-deploy/deploy.sh <branch>
```

This is the core mechanism. It is **not** `toJSON(secrets)` and **not** a hardcoded
list — it is a **name-driven loop** over your env vars producing three coordinated
pieces:

- **`env:`** puts each secret's value into the environment of the
  `appleboy/ssh-action` step *on the GitHub runner*.
- **`envs: VAR,DEPLOY_DIR,MOUNTABO_ENV_KEYS`** is that action's **allow-list** of which
  of the step's environment variables to **forward across the SSH connection** into the
  remote shell that runs `deploy.sh`.
- **`MOUNTABO_ENV_KEYS`** is the space-separated list of operator names, so `deploy.sh`
  assembles `.env` generically (see step 3).

So the value hops: `secrets.VAR` → runner step `env` → `envs:` forwards it over SSH →
`$VAR` in the shell on your server.

`SERVER_HOST` / `SERVER_USER` / `SERVER_SSH_KEY` are **not** forwarded to the script —
they are consumed directly by the scp/ssh action inputs to connect. `DEPLOY_DIR`
**is** forwarded, because it is appended to the names list.

### 3. `deploy.sh` assembles a `.env` file from the forwarded shell env

`deploy.sh` loops the forwarded `$MOUNTABO_ENV_KEYS` list, writing one `.env` line per
name from the matching forwarded secret — so no env-var names are baked into the
script (only the workflow carries them, which is what lets a later secret change be
applied by regenerating just the workflow):

```bash
for k in ${MOUNTABO_ENV_KEYS:-}; do
  v="${!k-}"
  if [ -z "$v" ]; then echo "warning: env var $k is empty"; continue; fi
  printf '%s=%s\n' "$k" "$v" >> .env
done
```

At runtime `${!k}` expands the forwarded shell var named by `$k`, so the loop writes
`VAR=val` into `.env` on the server.

### 4. The container receives it (compose vs docker differ)

| | Compose (`composeScript`) | Docker (`dockerScript`) |
|---|---|---|
| `.env` written | `cat > .env <<EOF …` — **with** port vars | `cat > .env <<EOF …` — **without** port vars |
| Injected via | `docker compose up -d` reads `.env` **implicitly** (also copied into each service's `env_file:`) | **explicit** `docker run --env-file .env` |
| Ports | shell `export PORT=…` → compose `${PORT}` substitution | `-p host:container` flags |

## End-to-end

```
UI  VAR=val
 └─► GitHub env secret  VAR  (encrypted; only place the value leaves Berth)
      └─► workflow step  env: VAR: ${{ secrets.VAR }}   (on the runner)
           └─► appleboy/ssh-action  envs: VAR   (forwarded over SSH)
                └─► remote shell  $VAR=val
                     └─► deploy.sh  cat > .env :  VAR=val
                          └─► container  (compose reads .env / docker --env-file .env)
```

The committed `deploy.sh` and workflow YAML contain **only the name `VAR`** — never
`val`. The value exists only as an encrypted GitHub secret and as ephemeral env/file
state on the runner and your server.

## Notes

- **Changing a secret later triggers a redeploy.** Adding, editing, or deleting a
  variable in the secrets tab writes it to GitHub, regenerates the committed workflow +
  `deploy.sh`, and fires a `workflow_dispatch` so the running container actually picks
  it up. The UI reports whether the redeploy was triggered; a deployment created before
  this feature needs one full redeploy from the configure page first (it has no
  persisted deploy config to regenerate from).
- **`.env` is written per line as `KEY=VALUE`.** Values reach `--env-file` / compose
  `.env` verbatim, so a value with spaces, `#`, or `=` is fine, but a **newline** can't
  be represented — multiline values (e.g. a PEM pasted as an app variable) are rejected
  by the secrets tab up front.

