FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /yggpeers ./cmd/yggpeers

FROM alpine:latest
RUN apk add --no-cache git ca-certificates
COPY --from=build /yggpeers /usr/local/bin/yggpeers
EXPOSE 8080
ENTRYPOINT ["yggpeers"]
