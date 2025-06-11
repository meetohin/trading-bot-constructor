# Многоступенчатая сборка для оптимизации размера образа

# Этап 1: Сборка приложения
FROM golang:1.21-alpine AS builder

# Устанавливаем необходимые пакеты
RUN apk add --no-cache git gcc musl-dev

# Создаем рабочую директорию
WORKDIR /app

# Копируем go.mod и go.sum для кэширования зависимостей
COPY go.mod go.sum ./

# Загружаем зависимости
RUN go mod download

# Копируем исходный код
COPY ../InvestTinkoff/invest-api-go-sdk/trading-server .

# Собираем приложение
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o trading-server .

# Этап 2: Финальный образ
FROM alpine:3.18

# Устанавливаем сертификаты для HTTPS запросов
RUN apk --no-cache add ca-certificates tzdata

# Создаем пользователя для запуска приложения
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

# Создаем необходимые директории
RUN mkdir -p /app/logs /app/web/static /app/web/templates && \
    chown -R appuser:appgroup /app

# Переключаемся на рабочую директорию
WORKDIR /app

# Копируем бинарник из builder
COPY --from=builder --chown=appuser:appgroup /app/trading-server .

# Копируем веб-ресурсы
COPY --chown=appuser:appgroup web ./web/

# Копируем конфигурационные файлы
COPY --chown=appuser:appgroup config.example.yaml ./config.example.yaml

# Устанавливаем права на выполнение
RUN chmod +x trading-server

# Переключаемся на непривилегированного пользователя
USER appuser

# Открываем порты
EXPOSE 8080 9090 8081

# Добавляем health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8081/health || exit 1

# Точка входа
ENTRYPOINT ["./trading-server"]

---

# Dockerfile для разработки
# Dockerfile.dev
FROM golang:1.21-alpine

# Устанавливаем необходимые инструменты для разработки
RUN apk add --no-cache git gcc musl-dev curl

# Устанавливаем Air для hot reload
RUN go install github.com/cosmtrek/air@latest

# Устанавливаем Delve для отладки
RUN go install github.com/go-delve/delve/cmd/dlv@latest

WORKDIR /app

# Копируем go.mod и go.sum
COPY go.mod go.sum ./

# Загружаем зависимости
RUN go mod download

# Копируем конфигурацию Air
COPY .air.toml ./

# Открываем порты
EXPOSE 8080 2345

# Команда по умолчанию для разработки
CMD ["air"]

---

# Dockerfile для стримов
# Dockerfile.streams
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY ../InvestTinkoff/invest-api-go-sdk/trading-server .

# Собираем сервис стримов
RUN CGO_ENABLED=0 GOOS=linux go build -o stream-service ./cmd/streams/

FROM alpine:3.18

RUN apk --no-cache add ca-certificates
RUN adduser -D -s /bin/sh appuser

WORKDIR /app

COPY --from=builder /app/stream-service .
COPY --chown=appuser:appuser config.example.yaml ./config.example.yaml

USER appuser

EXPOSE 8082

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8082/health || exit 1

CMD ["./stream-service"]

---

# Dockerfile для обработки ордеров
# Dockerfile.orders
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git gcc musl-dev

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY ../InvestTinkoff/invest-api-go-sdk/trading-server .

# Собираем сервис ордеров
RUN CGO_ENABLED=0 GOOS=linux go build -o order-service ./cmd/orders/

FROM alpine:3.18

RUN apk --no-cache add ca-certificates
RUN adduser -D -s /bin/sh appuser

WORKDIR /app

COPY --from=builder /app/order-service .
COPY --chown=appuser:appuser config.example.yaml ./config.example.yaml

USER appuser

EXPOSE 8083

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8083/health || exit 1

CMD ["./order-service"]

---

# .dockerignore
.git
.gitignore
README.md
Dockerfile*
docker-compose*
.env*
.air.toml
*.log
logs/
tmp/
.DS_Store
node_modules/
coverage.out
*.test
*.prof