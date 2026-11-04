FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/invoice-api . \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadgen ./cmd/loadgen

# Distroless "nonroot": no shell, no package manager, UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS loadgen
COPY --from=build /out/loadgen /loadgen
USER nonroot:nonroot
ENTRYPOINT ["/loadgen"]

# Last stage = default target: the API.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS app
COPY --from=build /out/invoice-api /invoice-api
USER nonroot:nonroot
EXPOSE 8080 9091
ENTRYPOINT ["/invoice-api"]
