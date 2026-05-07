FRONTEND_DIR = ./web
BACKEND_DIR = .
MIGRATIONS_DIR = ./migrations

.PHONY: all build-frontend start-backend \
        migrate-up migrate-up-by-one migrate-down migrate-down-to \
        migrate-status migrate-version migrate-redo migrate-reset \
        migrate-create migrate-validate \
        test test-cover

all: build-frontend start-backend

build-frontend:
	@echo "Building frontend..."
	@cd $(FRONTEND_DIR) && bun install && DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$(cat VERSION) bun run build

start-backend:
	@echo "Starting backend dev server..."
	@cd $(BACKEND_DIR) && go run main.go &

# ─── Database migrations (goose, per ADR-008) ──────────────────────────────
# Requires DATABASE_URL env var. See docs/migrations.md for the policy split
# between goose (new tables) and GORM AutoMigrate (inherited new-api tables).

migrate-up:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) up

migrate-up-by-one:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) up-by-one

migrate-down:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) down

migrate-down-to:
	@test -n "$(VERSION)" || (echo "VERSION=N is required"; exit 2)
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) down-to $(VERSION)

migrate-status:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) status

migrate-version:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) version

migrate-redo:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) redo

migrate-reset:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) reset

migrate-create:
	@test -n "$(NAME)" || (echo "NAME=<snake_case_name> is required"; exit 2)
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) create $(NAME) sql

migrate-validate:
	@go run ./cmd/migrate -dir $(MIGRATIONS_DIR) validate

# ─── Tests ─────────────────────────────────────────────────────────────────

test:
	@go test ./internal/...

test-cover:
	@go test -cover ./internal/...
