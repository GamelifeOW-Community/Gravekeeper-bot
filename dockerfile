# Сборка
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bot .

# Минимальный образ для запуска
FROM alpine:latest
WORKDIR /app
# Сертификаты нужны для HTTPS запросов к Discord API
RUN apk --no-cache add ca-certificates tzdata

COPY --from=builder /app/bot .

# Запуск бота
CMD ["./bot"]
