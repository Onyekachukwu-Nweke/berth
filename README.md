# Mountabo

Mountabo helps you deploy your app to your own server.

You run it locally, connect GitHub, add a VPS, and configure a repo once. After that, pushes to your chosen branch deploy through GitHub Actions straight to your server.

## Showcase

![Repo picker on the home page, with visibility and container filters](docs/screenshots/repos.png)

Mountabo deploys any repository with a `Dockerfile` or a Docker Compose setup, whatever the stack. Pick one in the picker, choose a server, and deploy.

## What You Need

- Docker (for Postgres locally, and for the hosted stack)
- Go and Node.js with npm, for local development
- a Linux VPS you can SSH into as `root`
- a repo with either a `Dockerfile` or a Docker Compose setup

If you want custom domains:

- DNS pointing at your server
- ports `80` and `443` open

## Setup

### 1. Install dependencies

```bash
make deps
```

### 2. Create `.env`

```bash
cp .env.example .env
```

Fill in:

```env
GITHUB_CLIENT_ID=...
GITHUB_CLIENT_SECRET=...
MOUNTABO_BACKEND=http://localhost:7778
MOUNTABO_HTTP_ADDR=127.0.0.1:7778

# Postgres holds all state. `make mountabo` starts a local one for you.
DATABASE_URL=postgres://mountabo:mountabo@localhost:5432/mountabo?sslmode=disable

# Seals server SSH keys in the database. Back it up.
# openssl rand -base64 32
MOUNTABO_MASTER_KEY=...

# Bring your own AI key for the terminal page's AI helper. Without it the
# terminal still works, but the helper shows a hint to set the key. Grab one
# at https://console.anthropic.com.
ANTHROPIC_API_KEY=...
```

The AI helper is bring your own key: it calls Anthropic directly from your backend, and you pay for your own usage.

### 3. Start Mountabo

```bash
make mountabo
```

This starts a local Postgres (`make db`), the backend and the frontend. Then open [http://localhost:4321](http://localhost:4321).

## Hosting Mountabo on a server

The hosted stack is Postgres, the Go API (private, never published) and the web app:

```bash
cp .env.example .env   # set POSTGRES_PASSWORD, MOUNTABO_MASTER_KEY, MOUNTABO_PUBLIC_URL, the GitHub App, MOUNTABO_ADMINS
docker compose up -d --build
```

Put a TLS reverse proxy in front of port `4321` and point `MOUNTABO_PUBLIC_URL` at it; the GitHub App's callback URL is `$MOUNTABO_PUBLIC_URL/api/auth/callback`. Back up `MOUNTABO_MASTER_KEY` and the `pgdata` volume together: one is useless without the other.

## Who can do what

Everyone signs in with GitHub. There are no Mountabo passwords and no user list to keep in sync.

- **Repo access is GitHub's.** A person sees the repos they can reach on GitHub, wherever the Mountabo GitHub App is installed, including repos they're only an outside collaborator on. Write (push) access is needed to deploy; read access can view deployments. Removing someone from a repo on GitHub removes it in Mountabo within ten minutes.
- **Where code runs is Mountabo's.** Admins (`MOUNTABO_ADMINS`) create **clients**, mark each server as shared (the agency's, e.g. a dev box) or owned by one client, link repos to clients (a whole client org with `*`, or single repos), and set each client's **environments** (e.g. Acme `development` → the shared dev box, Acme `production` → Acme's own server).
- A developer picks an environment; the server follows from the rule. A client's server only ever runs that client's repos, for admins too.
- Server management, the terminal and server logs are admin-only.

Set this up under **access** in the header (admins only).

## GitHub Setup

Make sure your GitHub connection settings are present in `.env`, then connect your account from the app.

Mountabo asks for:

- `repo`
- `workflow`

## How To Use It

### 1. Connect GitHub

Open the app and connect your GitHub account.

### 2. Add a server

Enter:

- server name
- IP address
- SSH port
- timezone
- root password
- optional personal SSH public key

### 3. Run server setup

Mountabo prepares the box for deploys by creating the `mountabo` user, installing SSH keys, and installing Docker.

You can also turn on optional extras like firewall rules, fail2ban, monitoring tools, and SSH hardening.

### 4. Configure a repo

Pick:

- repository
- branch
- server
- deploy strategy
- deploy directory
- root directory if the app is inside a subfolder
- environment variables
- port mappings

Mountabo previews what it will set up, then writes the deploy workflow and required secrets to GitHub.

### 5. Push code

Push to the configured branch and GitHub Actions will deploy it to your server.

Mountabo does not need to stay open for deploys to run.

### 6. Monitor deployments

Use the monitor screen to see configured deployments, recent GitHub Actions runs, and basic server metrics.

### 7. Add a domain

For a ready server, Mountabo can configure nginx and Let's Encrypt for a custom domain pointing at one of your app ports.

Each server's domains run behind **nginx** (the default: nginx sites plus certbot) or, optionally, **Traefik**. Switch under **Servers**, by expanding a ready server and choosing **Reverse proxy** (admins only). Traefik runs as the `mountabo-traefik` container on ports 80 and 443 with host networking. Its config lives in `/etc/mountabo/traefik/`: `traefik.yml` (static config) plus one route file per domain in `dynamic/`. It issues and renews Let's Encrypt certificates itself, and adding or removing a domain doesn't reload anything. Switching either way moves every existing domain across; the new proxy issues fresh certificates, so DNS must still point at the server.

## Templates

**Templates** (in the sidebar) is a small library of Dockerfile and `docker-compose.yml` starting points that already follow what Mountabo's deploys expect. The library covers Next.js, a Node.js API, FastAPI, Django, Laravel, plain PHP, Go, and a static site or SPA.

Every template uses the same conventions:
- The app port is published as `${APP_PORT:-<port>}:<port>`.
- Configuration comes from an optional `.env` that the deploy writes from GitHub secrets.
- The app runs as a non-root user.

With a template you can:
- **Edit** the files in place.
- **Copy AI prompt** to get a prompt that spells out these rules and includes the template and, if you picked a repo, its file list. Paste it into your AI to adapt the setup to the project.
- **Commit to repo** to write the files onto a branch with the GitHub App. This needs push access, and it warns you before replacing files that already exist.

Repos with no Docker setup show an **Add Docker setup** link on New deployment.

## Terminal

![Terminal page with the AI helper](docs/screenshots/terminal.png)

Open `/terminal` to get a web shell on any of your set up servers.

- Pick a server from the dropdown; the prompt becomes `mountabo@<server> <cwd> %`.
- Type a shell command and run it. The command runs as the `mountabo` user over SSH, with the working directory carried between commands so `cd` and relative paths feel persistent.
- By default each command goes through a confirm step. Flip `express mode` on to run commands immediately without the per command confirm.

### AI helper

Below the terminal there is an AI helper. Describe what you want in plain English (say, "I want to know if there are any containers running") and it suggests a shell command with a short explanation. Click `use this command` to fill the terminal above with the suggestion, then review and run it yourself. The helper only suggests; nothing it returns is ever executed automatically.

To enable the helper, set `ANTHROPIC_API_KEY` in your `.env` to a key from [the Anthropic console](https://console.anthropic.com) and restart the backend. Without it, the terminal still works and the helper shows a hint to set the key.

## Notes

- Locally, the backend listens on `127.0.0.1:7778` and the frontend on `http://localhost:4321`.
- All state lives in Postgres. Server SSH keys are sealed with `MOUNTABO_MASTER_KEY`; app secrets are never stored by Mountabo, they go straight to GitHub Actions secrets. Your source code never goes to servers: the runner builds the images and ships them with just the compose files.
- The current setup flow is aimed at fresh Ubuntu-style servers.
