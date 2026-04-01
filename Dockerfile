FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY ./ .

RUN go mod download && go build -o main main.go

FROM alpine:3.23.3

WORKDIR /root/

COPY --from=builder /app/main .

CMD ["./main"]
