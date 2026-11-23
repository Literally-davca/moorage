# syntax=docker/dockerfile:1
# Moorage: one static binary in a scratch image.

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web /src/internal/web/dist ./internal/web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /moorage ./cmd/moorage
RUN mkdir -p /out/data /out/tmp && chmod 1777 /out/tmp

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/ /
COPY --from=build /moorage /moorage
ENV MOORAGE_DATA_DIR=/data MOORAGE_STACKS_DIR=/opt/stacks MOORAGE_LISTEN=:5080 TMPDIR=/tmp
EXPOSE 5080 5081
VOLUME ["/data"]
ENTRYPOINT ["/moorage"]
