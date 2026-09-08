# Conductor — control plane (stdlib-only Go service).
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o conductor .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /app/conductor .
ENV PORT=8090
# Persist the rules across restarts — mount a CapRover volume here.
ENV CONDUCTOR_DATA_DIR=/data
EXPOSE 8090
CMD ["./conductor"]
