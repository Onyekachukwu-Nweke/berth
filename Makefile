.PHONY: berth mountabo backend frontend deps db db-stop up down

# Start the whole project: backend + frontend together.
# Ctrl-C stops both.
berth: db
	@echo "starting berth (backend + frontend)..."
	@trap 'kill 0' EXIT INT TERM; \
	$(MAKE) backend & \
	$(MAKE) frontend & \
	wait

# Old name, kept so `make mountabo` still works.
mountabo: berth

backend:
	@cd backend && go run ./cmd/server

frontend:
	@cd frontend && npm run dev

deps:
	@cd backend && go mod download
	@cd frontend && npm install

# Local Postgres for development, matching DATABASE_URL in .env.example.
db:
	@if docker inspect mountabo-db >/dev/null 2>&1; then \
		docker start mountabo-db >/dev/null && echo "postgres on localhost:5432 (existing mountabo-db container kept from before the rename)"; \
	else \
		docker start berth-db >/dev/null 2>&1 || docker run -d --name berth-db \
			-e POSTGRES_DB=berth -e POSTGRES_USER=berth -e POSTGRES_PASSWORD=berth \
			-p 5432:5432 -v berth-db:/var/lib/postgresql/data postgres:17-alpine >/dev/null; \
		echo "postgres on localhost:5432 (db/user/pass: berth)"; \
	fi

db-stop:
	@docker stop mountabo-db berth-db >/dev/null 2>&1; echo "postgres stopped"

# The hosted stack: postgres + api + web, from docker-compose.yml.
up:
	@docker compose up -d --build

down:
	@docker compose down
