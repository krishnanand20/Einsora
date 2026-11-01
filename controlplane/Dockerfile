# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/einsora ./cmd/einsora

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/einsora /usr/local/bin/einsora
ENV EINSORA_HTTP_ADDR=:8080 \
    EINSORA_CATALOG_PATH=/etc/einsora/catalog.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/einsora"]
CMD ["api"]
