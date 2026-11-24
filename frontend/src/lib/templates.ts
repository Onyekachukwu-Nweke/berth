// The Docker template library: starting points that already follow the
// conventions Mountabo's deploys rely on. Edit them in the UI, hand them to an
// AI with the generated prompt, or commit them straight into a repo.

export type TemplateFile = { path: string; content: string };

export type Template = {
  id: string;
  name: string;
  stack: string;
  description: string;
  port: number;
  notes: string[];
  files: TemplateFile[];
};

// Every compose file publishes the app as ${APP_PORT:-<port>} (Mountabo sets
// the host port per environment) and reads .env, which deploy.sh writes from
// the environment's GitHub secrets. The .env is optional so the image still
// builds on the Actions runner, where it doesn't exist.
function compose(port: number, containerPort = port, extra = ""): string {
  return `services:
  app:
    build: .
    restart: unless-stopped
${extra}    ports:
      - "\${APP_PORT:-${port}}:${containerPort}"
    env_file:
      - path: .env
        required: false
`;
}

const nodeIgnore = `node_modules
npm-debug.log*
.next
dist
build
coverage
.git
.env
.env.*
!.env.example
Dockerfile
docker-compose.yml
`;

const pythonIgnore = `__pycache__/
*.py[cod]
.venv
venv
.pytest_cache
.mypy_cache
.git
.env
.env.*
!.env.example
Dockerfile
docker-compose.yml
`;

const phpIgnore = `vendor
node_modules
public/build
public/hot
bootstrap/cache/*.php
storage/*.key
storage/logs/*
storage/framework/cache/*
storage/framework/sessions/*
storage/framework/views/*
.phpunit.result.cache
.git
.env
.env.*
!.env.example
Dockerfile
docker-compose.yml
`;

export const TEMPLATES: Template[] = [
  {
    id: "nextjs",
    name: "Next.js",
    stack: "Node 22 · standalone output",
    description: "A Next.js app built once and run from its standalone server, as a non-root user.",
    port: 3000,
    notes: [
      'Set output: "standalone" in next.config.',
      "Uses npm, pnpm or yarn, whichever lockfile the repo has.",
      "Remove the public/ COPY line if the app has no public folder.",
      "NEXT_PUBLIC_* variables are baked in at build time, so they must exist when the image builds.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* ./
RUN if [ -f pnpm-lock.yaml ]; then corepack enable && pnpm install --frozen-lockfile; \\
    elif [ -f yarn.lock ]; then corepack enable && yarn install --immutable; \\
    else npm ci; fi

FROM node:22-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 PORT=3000 HOSTNAME=0.0.0.0
COPY --from=build --chown=node:node /app/public ./public
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
USER node
EXPOSE 3000
CMD ["node", "server.js"]
`,
      },
      { path: "docker-compose.yml", content: compose(3000) },
      { path: ".dockerignore", content: nodeIgnore },
    ],
  },
  {
    id: "node-api",
    name: "Node.js API",
    stack: "Node 22 · Express, Fastify, Nest…",
    description: "A Node service: install, build if there's a build script, drop dev dependencies, run npm start.",
    port: 8080,
    notes: [
      "The app must listen on process.env.PORT (8080 here) and on 0.0.0.0.",
      'Runs "npm start"; point that script at the built entry (e.g. node dist/main.js).',
      "init: true gives the process a proper PID 1, so stop signals reach it.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build --if-present && npm prune --omit=dev

FROM node:22-alpine
WORKDIR /app
ENV NODE_ENV=production PORT=8080
COPY --from=build --chown=node:node /app ./
USER node
EXPOSE 8080
CMD ["npm", "start"]
`,
      },
      { path: "docker-compose.yml", content: compose(8080, 8080, "    init: true\n") },
      { path: ".dockerignore", content: nodeIgnore },
    ],
  },
  {
    id: "fastapi",
    name: "Python · FastAPI",
    stack: "Python 3.13 · uvicorn",
    description: "A FastAPI (or any ASGI) app served by uvicorn, as a non-root user.",
    port: 8000,
    notes: [
      "Expects requirements.txt with uvicorn in it.",
      'Change "main:app" to your module and app object (e.g. "app.main:app").',
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM python:3.13-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
RUN useradd --create-home --uid 10001 app && chown app:app /app
COPY requirements.txt .
RUN pip install -r requirements.txt
COPY --chown=app:app . .
USER app
EXPOSE 8000
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000"]
`,
      },
      { path: "docker-compose.yml", content: compose(8000) },
      { path: ".dockerignore", content: pythonIgnore },
    ],
  },
  {
    id: "django",
    name: "Python · Django",
    stack: "Python 3.13 · gunicorn",
    description: "A Django project behind gunicorn. Static files are collected when the container starts.",
    port: 8000,
    notes: [
      'Replace "config" with your project package (the folder with wsgi.py).',
      "Run migrations as a Mountabo deploy hook (python manage.py migrate), not in the image.",
      "Serve static files with WhiteNoise, or point a domain path at them.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM python:3.13-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
RUN useradd --create-home --uid 10001 app && chown app:app /app
COPY requirements.txt .
RUN pip install -r requirements.txt gunicorn
COPY --chown=app:app . .
USER app
EXPOSE 8000
CMD ["sh", "-c", "python manage.py collectstatic --noinput && exec gunicorn config.wsgi:application --bind 0.0.0.0:8000 --workers 3 --access-logfile -"]
`,
      },
      { path: "docker-compose.yml", content: compose(8000) },
      { path: ".dockerignore", content: pythonIgnore },
    ],
  },
  {
    id: "laravel",
    name: "PHP · Laravel",
    stack: "PHP 8.4 · FrankenPHP",
    description: "A Laravel app: Composer and Vite build stages, served by FrankenPHP as a non-root user.",
    port: 8080,
    notes: [
      "Set APP_KEY (php artisan key:generate --show) and your DB_* variables as environment variables.",
      "Run migrations as a Mountabo deploy hook (php artisan migrate --force), not in the image.",
      "Config, routes and views are cached at start, when the real environment is present.",
      "No frontend? Delete the assets stage and its COPY line.",
      "Queues or the scheduler run as extra compose services from the same image (php artisan queue:work, schedule:work).",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM composer:2 AS vendor
WORKDIR /app
COPY composer.json composer.lock ./
# Extensions live in the runtime image, so skip the platform check here.
RUN composer install --no-dev --no-scripts --no-autoloader --prefer-dist --no-interaction --ignore-platform-reqs
COPY . .
# Package manifests cached on a dev machine list dev-only packages; rebuild them.
RUN rm -f bootstrap/cache/*.php && composer dump-autoload --optimize --no-dev --no-scripts

FROM node:22-alpine AS assets
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

FROM dunglas/frankenphp:1-php8.4
RUN install-php-extensions pdo_mysql pdo_pgsql intl zip bcmath opcache pcntl
WORKDIR /app
RUN useradd --create-home --uid 10001 app && chown -R app:app /app /data/caddy /config/caddy
COPY --chown=app:app --from=vendor /app ./
COPY --chown=app:app --from=assets /app/public/build ./public/build
ENV APP_ENV=production LOG_CHANNEL=stderr
USER app
RUN php artisan package:discover --ansi
EXPOSE 8080
CMD ["sh", "-c", "php artisan config:cache && php artisan route:cache && php artisan view:cache && exec frankenphp php-server --listen :8080 --root /app/public"]
`,
      },
      { path: "docker-compose.yml", content: compose(8080) },
      { path: ".dockerignore", content: phpIgnore },
    ],
  },
  {
    id: "php",
    name: "PHP",
    stack: "PHP 8.4 · FrankenPHP",
    description: "A plain PHP app (or Symfony, Slim…) served by FrankenPHP, with Composer if the project uses it.",
    port: 8080,
    notes: [
      'Serves /app; change --root to /app/public if your entry point lives there (Symfony, Slim).',
      "Add the extensions you need to the install-php-extensions line.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM composer:2 AS vendor
WORKDIR /app
COPY . .
RUN if [ -f composer.json ]; then \
      composer install --no-dev --prefer-dist --no-interaction --optimize-autoloader --ignore-platform-reqs; \
    fi

FROM dunglas/frankenphp:1-php8.4
RUN install-php-extensions pdo_mysql pdo_pgsql opcache
WORKDIR /app
RUN useradd --create-home --uid 10001 app && chown -R app:app /app /data/caddy /config/caddy
COPY --chown=app:app --from=vendor /app ./
USER app
EXPOSE 8080
CMD ["frankenphp", "php-server", "--listen", ":8080", "--root", "/app"]
`,
      },
      { path: "docker-compose.yml", content: compose(8080) },
      { path: ".dockerignore", content: phpIgnore },
    ],
  },
  {
    id: "go",
    name: "Go",
    stack: "Go 1.26 · distroless",
    description: "A static Go binary in a distroless image: tiny, no shell, runs as non-root.",
    port: 8080,
    notes: [
      'Change "." to your main package if it lives elsewhere (e.g. ./cmd/server).',
      "The server must listen on 0.0.0.0:8080 (or read the port from an env var).",
      "Distroless has no shell, so docker exec into it won't work; that's intended.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
`,
      },
      { path: "docker-compose.yml", content: compose(8080) },
      {
        path: ".dockerignore",
        content: `.git
.env
.env.*
!.env.example
bin
dist
Dockerfile
docker-compose.yml
`,
      },
    ],
  },
  {
    id: "static-spa",
    name: "Static site / SPA",
    stack: "Vite, React, Vue · nginx",
    description: "Builds the site with Node and serves the files from a small nginx image, with SPA routing.",
    port: 8080,
    notes: [
      "Expects the build to land in dist/ (Vite's default); change it for other tools.",
      "VITE_* variables are baked in at build time.",
      "The nginx here only serves files inside the container; your server's proxy still handles domains and HTTPS.",
    ],
    files: [
      {
        path: "Dockerfile",
        content: `# syntax=docker/dockerfile:1
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm ci
COPY . .
RUN npm run build

# The unprivileged image runs nginx as a non-root user on port 8080.
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 8080
`,
      },
      {
        path: "nginx.conf",
        content: `server {
  listen 8080;
  root /usr/share/nginx/html;
  index index.html;

  # Hashed build assets never change, so cache them for a year.
  location /assets/ {
    expires 1y;
    add_header Cache-Control "public, immutable";
    try_files $uri =404;
  }

  # Everything else falls back to index.html for client-side routing.
  location / {
    try_files $uri $uri/ /index.html;
  }
}
`,
      },
      { path: "docker-compose.yml", content: compose(8080) },
      { path: ".dockerignore", content: nodeIgnore },
    ],
  },
];

export function findTemplate(id: string | null | undefined): Template | undefined {
  return TEMPLATES.find((t) => t.id === id);
}

// suggestTemplate picks the likeliest template from a repo's file list.
export function suggestTemplate(paths: string[]): Template | undefined {
  const has = (re: RegExp) => paths.some((p) => re.test(p));
  // Laravel ships a package.json too, so it must win before the Node checks.
  if (has(/^artisan$/)) return findTemplate("laravel");
  if (has(/^next\.config\.(js|mjs|ts)$/)) return findTemplate("nextjs");
  if (has(/^manage\.py$/)) return findTemplate("django");
  if (has(/^go\.mod$/)) return findTemplate("go");
  if (has(/^(requirements\.txt|pyproject\.toml)$/)) return findTemplate("fastapi");
  if (has(/^(composer\.json|index\.php)$/)) return findTemplate("php");
  if (has(/^vite\.config\.(js|mjs|ts)$/)) return findTemplate("static-spa");
  if (has(/^package\.json$/)) return findTemplate("node-api");
  return undefined;
}

// buildAIPrompt asks an AI to adapt the (possibly edited) template to a real
// repo, spelling out what Mountabo needs so the answer deploys first time.
export function buildAIPrompt(t: Template, files: TemplateFile[], repo?: { name: string; branch: string; paths: string[] }) {
  const fence = (f: TemplateFile) => `--- ${f.path} ---\n${f.content.trimEnd()}\n`;
  const tree = repo?.paths.length
    ? `\nThe repository's files (first ${Math.min(repo.paths.length, 300)}):\n${repo.paths.slice(0, 300).join("\n")}\n`
    : "";
  return `I need a Docker setup for ${repo ? `the repository ${repo.name} (branch ${repo.branch})` : "my project"} so it can be deployed with Mountabo. Adapt the starting files below to the project. Read the project's own files (package manifest, lockfile, entry point, build output, framework config) instead of guessing.

What Mountabo needs:
1. A Dockerfile and a docker-compose.yml at the project root. GitHub Actions builds the images on an ubuntu runner with \`docker compose build\`, ships them to the server, and starts them with \`docker compose up -d\`.
2. The app's service publishes its port as "\${APP_PORT:-<default>}:<container port>". Mountabo sets APP_PORT per environment, so keep that exact form.
3. Configuration comes from environment variables only. Mountabo writes them to a .env file from GitHub secrets, and compose loads it with \`env_file: [{path: .env, required: false}]\`. Never bake secrets into the image, and keep .env in .dockerignore.
4. The app listens on 0.0.0.0, not localhost.
5. Run as a non-root user, use multi-stage builds where it helps, and pin base image major versions.
6. Database migrations don't belong in the image or the start command. Mountabo runs them as deploy hooks, so tell me the command instead.
7. If the app needs a database or cache, add it as another compose service with a named volume. Don't publish its port, and read its credentials from .env.

Starting point: the "${t.name}" template (${t.stack}).
${t.notes.length ? `Notes on this template:\n${t.notes.map((n) => `- ${n}`).join("\n")}\n` : ""}
${files.map(fence).join("\n")}${tree}
Reply with:
- each file in full, ready to commit
- the environment variables the app needs, each with a one-line description
- the migration command, if there is one
- anything in the project I should change for this to work`;
}
