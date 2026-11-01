# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tensorafi ./cmd/tensorafi

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tensorafi /usr/local/bin/tensorafi
ENV TENSORAFI_HTTP_ADDR=:8080 \
    TENSORAFI_CATALOG_PATH=/etc/tensorafi/catalog.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/tensorafi"]
CMD ["api"]
