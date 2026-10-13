# syntax=docker/dockerfile:1
# Builds g711-radio as a static binary in a minimal, non-root image. The same
# image runs an origin or an edge; config.json decides (see README).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/g711-radio .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/g711-radio /usr/local/bin/g711-radio
# config.json, config.secrets.json, certificates, the log file and analytics
# all live in the mounted /data directory; nothing secret is baked in.
WORKDIR /data
EXPOSE 8443/tcp 8080/tcp 3478/udp
ENTRYPOINT ["/usr/local/bin/g711-radio"]
