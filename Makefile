# Переменные
APP_NAME := trading-server
VERSION := $(shell git describe --tags --always --dirty)
BUILD_TIME := $(shell date +%Y-%m-%dT%H:%M:%S)
GO_VERSION := $(shell go version | awk '{print $$3}')

# Docker
DOCKER_REGISTRY := your-registry.com
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)
DOCKER_TAG := $(VERSION)

# Директории
BUILD_DIR := ./bin
COVERAGE_DIR := ./coverage

# Go параметры
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod
GOFMT := gofmt
GOLINT := golangci-lint

# Флаги сборки
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GoVersion=$(GO_VERSION)"

.PHONY: help
help: ## Показать помощь
	@echo "Доступные команды:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Собрать приложение
	@echo "Сборка $(APP_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME) .
	@echo "Сборка завершена: $(BUILD_DIR)/$(APP_NAME)"

.PHONY: build-all
build-all: ## Собрать все сервисы
	@echo "Сборка всех сервисов..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(APP_NAME) .
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/stream-service ./cmd/streams/
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/order-service ./cmd/orders/
	@echo "Сборка всех сервисов завершена"

.PHONY: run
run: build ## Запустить приложение
	@echo "Запуск $(APP_NAME)..."
	./$(BUILD_DIR)/$(APP_NAME)

.PHONY: dev
dev: ## Запустить в режиме разработки с hot reload
	@echo "Запуск в режиме разработки..."
	air

.PHONY: test
test: ## Запустить тесты
	@echo "Запуск тестов..."
	$(GOTEST) -v -race -timeout 30s ./...

.PHONY: test-coverage
test-coverage: ## Запустить тесты с покрытием
	@echo "Запуск тестов с покрытием..."
	@mkdir -p $(COVERAGE_DIR)
	$(GOTEST) -v -race -coverprofile=$(COVERAGE_DIR)/coverage.out -covermode=atomic ./...
	$(GOCMD) tool cover -html=$(COVERAGE_DIR)/coverage.out -o $(COVERAGE_DIR)/coverage.html
	@echo "Отчет о покрытии: $(COVERAGE_DIR)/coverage.html"

.PHONY: benchmark
benchmark: ## Запустить бенчмарки
	@echo "Запуск бенчмарков..."
	$(GOTEST) -bench=. -benchmem ./...

.PHONY: lint
lint: ## Запустить линтер
	@echo "Запуск линтера..."
	$(GOLINT) run ./...

.PHONY: fmt
fmt: ## Форматировать код
	@echo "Форматирование кода..."
	$(GOFMT) -w .
	$(GOCMD) mod tidy

.PHONY: clean
clean: ## Очистить артефакты сборки
	@echo "Очистка..."
	rm -rf $(BUILD_DIR)
	rm -rf $(COVERAGE_DIR)
	rm -rf tmp/
	$(GOCMD) clean

.PHONY: deps
deps: ## Установить зависимости
	@echo "Установка зависимостей..."
	$(GOGET) -v ./...
	$(GOMOD) tidy

.PHONY: deps-update
deps-update: ## Обновить зависимости
	@echo "Обновление зависимостей..."
	$(GOGET) -u ./...
	$(GOMOD) tidy

.PHONY: docker-build
docker-build: ## Собрать Docker образ
	@echo "Сборка Docker образа..."
	docker build -t $(APP_NAME):$(DOCKER_TAG) .
	docker tag $(APP_NAME):$(DOCKER_TAG) $(APP_NAME):latest

.PHONY: docker-build-all
docker-build-all: ## Собрать все Docker образы
	@echo "Сборка всех Docker образов..."
	docker build -t $(APP_NAME):$(DOCKER_TAG) .
	docker build -f Dockerfile.streams -t $(APP_NAME)-streams:$(DOCKER_TAG) .
	docker build -f Dockerfile.orders -t $(APP_NAME)-orders:$(DOCKER_TAG) .

.PHONY: docker-push
docker-push: docker-build ## Отправить Docker образ в реестр
	@echo "Отправка Docker образа..."
	docker tag $(APP_NAME):$(DOCKER_TAG) $(DOCKER_IMAGE):$(DOCKER_TAG)
	docker tag $(APP_NAME):$(DOCKER_TAG) $(DOCKER_IMAGE):latest
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)
	docker push $(DOCKER_IMAGE):latest

.PHONY: docker-run
docker-run: ## Запустить Docker контейнер
	@echo "Запуск Docker контейнера..."
	docker run --rm -p 8080:8080 -v $(PWD)/config.yaml:/app/config.yaml $(APP_NAME):latest

.PHONY: compose-up
compose-up: ## Запустить через Docker Compose
	@echo "Запуск через Docker Compose..."
	docker-compose up -d

.PHONY: compose-down
compose-down: ## Остановить Docker Compose
	@echo "Остановка Docker Compose..."
	docker-compose down

.PHONY: compose-logs
compose-logs: ## Показать логи Docker Compose
	docker-compose logs -f

.PHONY: compose-restart
compose-restart: compose-down compose-up ## Перезапустить Docker Compose

.PHONY: config-check
config-check: ## Проверить конфигурацию
	@echo "Проверка конфигурации..."
	@if [ ! -f config.yaml ]; then \
		echo "Файл config.yaml не найден. Создайте его на основе config.example.yaml"; \
		exit 1; \
	fi
	@echo "Конфигурация найдена"

.PHONY: config-create
config-create: ## Создать конфигурацию из примера
	@echo "Создание конфигурации..."
	@if [ ! -f config.yaml ]; then \
		cp config.example.yaml config.yaml; \
		echo "Файл config.yaml создан. Отредактируйте его перед запуском"; \
	else \
		echo "Файл config.yaml уже существует"; \
	fi

.PHONY: install-tools
install-tools: ## Установить инструменты разработки
	@echo "Установка инструментов разработки..."
	$(GOGET) -u github.com/cosmtrek/air@latest
	$(GOGET) -u github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	$(GOGET) -u github.com/go-delve/delve/cmd/dlv@latest

.PHONY: gen-docs
gen-docs: ## Генерировать документацию API
	@echo "Генерация документации API..."
	@if command -v swag >/dev/null 2>&1; then \
		swag init -g main.go; \
	else \
		echo "Установите swag: go install github.com/swaggo/swag/cmd/swag@latest"; \
	fi

.PHONY: security-check
security-check: ## Проверка безопасности
	@echo "Проверка безопасности..."
	@if command -v gosec >/dev/null 2>&1; then \
		gosec ./...; \
	else \
		echo "Установите gosec: go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest"; \
	fi

.PHONY: migrate
migrate: ## Запустить миграции базы данных
	@echo "Запуск миграций..."
	@if command -v migrate >/dev/null 2>&1; then \
		migrate -path ./migrations -database "postgres://trading_user:trading_password@localhost:5432/trading_db?sslmode=disable" up; \
	else \
		echo "Установите migrate: go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest"; \
	fi

.PHONY: backup-db
backup-db: ## Создать бэкап базы данных
	@echo "Создание бэкапа базы данных..."
	@mkdir -p backups
	docker exec trading-server_postgres_1 pg_dump -U trading_user trading_db > backups/backup_$(shell date +%Y%m%d_%H%M%S).sql

.PHONY: restore-db
restore-db: ## Восстановить базу данных из бэкапа
	@echo "Восстановление базы данных..."
	@read -p "Введите путь к файлу бэкапа: " backup_file; \
	docker exec -i trading-server_postgres_1 psql -U trading_user -d trading_db < $$backup_file

.PHONY: logs
logs: ## Показать логи приложения
	@echo "Логи приложения..."
	tail -f logs/trading-server.log

.PHONY: monitor
monitor: ## Запустить мониторинг
	@echo "Мониторинг системы..."
	@echo "API Status:" && curl -s http://localhost:8080/api/v1/status | jq .
	@echo "Health Check:" && curl -s http://localhost:8081/health
	@echo "Metrics:" && curl -s http://localhost:9090/metrics | head -10

.PHONY: load-test
load-test: ## Запустить нагрузочное тестирование
	@echo "Нагрузочное тестирование..."
	@if command -v hey >/dev/null 2>&1; then \
		hey -n 1000 -c 10 http://localhost:8080/api/v1/status; \
	else \
		echo "Установите hey: go install github.com/rakyll/hey@latest"; \
	fi

.PHONY: deploy-staging
deploy-staging: docker-build ## Деплой на staging
	@echo "Деплой на staging..."
	# Добавьте команды для деплоя на staging

.PHONY: deploy-prod
deploy-prod: docker-push ## Деплой на production
	@echo "Деплой на production..."
	# Добавьте команды для деплоя на production

.PHONY: version
version: ## Показать версию
	@echo "Version: $(VERSION)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo "Go Version: $(GO_VERSION)"

# Алиасы для удобства
.PHONY: up down restart
up: compose-up
down: compose-down
restart: compose-restart