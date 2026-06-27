# Build the self-contained station binary (web/index.html is embedded).
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /station .

# Minimal runtime: just the static binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /station /station
EXPOSE 8080
ENTRYPOINT ["/station"]
