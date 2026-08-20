FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/flexdav ./cmd/bridge

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 app
COPY --from=build /out/flexdav /usr/local/bin/flexdav
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/flexdav"]
