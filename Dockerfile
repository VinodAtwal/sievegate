# Builds either the quality-test proxy (default) or the mock service.
#
#   docker build --build-arg PKG=.                       -t sievegate/proxy:latest .
#   docker build --build-arg PKG=./example/mockservice   -t sievegate/mock:latest .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG PKG=.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ${PKG}

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]