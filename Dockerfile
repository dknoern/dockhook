FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o dockhook .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/dockhook /usr/local/bin/dockhook
EXPOSE 8080
ENTRYPOINT ["dockhook"]
