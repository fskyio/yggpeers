FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
FROM build AS build-server
RUN apk add --no-cache gcc musl-dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /yggpeers ./cmd/yggpeers

FROM build AS build-probe
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /yggpeers-probe ./cmd/yggpeers-probe

FROM alpine:latest AS probe
RUN apk add --no-cache ca-certificates
COPY --from=build-probe /yggpeers-probe /usr/local/bin/yggpeers-probe
ENTRYPOINT ["yggpeers-probe"]

FROM alpine:latest AS server
RUN apk add --no-cache ca-certificates
COPY --from=build-server /yggpeers /usr/local/bin/yggpeers
EXPOSE 8080
ENTRYPOINT ["yggpeers"]
