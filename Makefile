.PHONY: mountabo backend frontend deps db db-stop up down

# Start the whole project: backend + frontend together.
# Ctrl-C stops both.
mountabo: db
	@echo "starting mountabo (backend + frontend)..."
	@trap 'kill 0' EXIT INT TERM; \
	$(MAKE) backend & \
	$(MAKE) frontend & \
	wait

backend:
	@cd backend && go run ./cmd/server

frontend:
	@cd frontend && npm run dev

deps:
	@cd backend && go mod download
	@cd frontend && npm install

# Local Postgres for development, matching DATABASE_URL in .env.example.
db:
	@docker start mountabo-db >/dev/null 2>&1 || docker run -d --name mountabo-db \
		-e POSTGRES_DB=mountabo -e POSTGRES_USER=mountabo -e POSTGRES_PASSWORD=mountabo \
		-p 5432:5432 -v mountabo-db:/var/lib/postgresql/data postgres:17-alpine >/dev/null
	@echo "postgres on localhost:5432 (db/user/pass: mountabo)"

db-stop:
	@docker stop mountabo-db >/dev/null && echo "postgres stopped"

# The hosted stack: postgres + api + web, from docker-compose.yml.
up:
	@docker compose up -d --build

down:
	@docker compose down
