FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY main.go .
RUN CGO_ENABLED=0 go build -o pprof-demo main.go

FROM alpine:3.21
COPY --from=builder /app/pprof-demo /usr/local/bin/pprof-demo
EXPOSE 6060
ENTRYPOINT ["/usr/local/bin/pprof-demo"]
