FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /yggpeers ./cmd/yggpeers

FROM alpine:latest
RUN apk add --no-cache git ca-certificates
COPY --from=build /yggpeers /usr/local/bin/yggpeers
EXPOSE 8080
ENTRYPOINT ["yggpeers"]
