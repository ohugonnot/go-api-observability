FROM golang:1.26.10-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /invoice-api .

# Distroless "nonroot": no shell, no package manager, UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /invoice-api /invoice-api
USER nonroot:nonroot
EXPOSE 8080 9091
ENTRYPOINT ["/invoice-api"]
