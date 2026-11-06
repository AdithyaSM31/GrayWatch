FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO_ENABLED=0 forces Go's pure resolver, which re-reads resolv.conf. The
# dns-break fault relies on that to take effect without a restart.
RUN CGO_ENABLED=0 go build -o /out/ ./cmd/svc ./cmd/dep

FROM alpine:3.20
COPY --from=build /out/svc /out/dep /
CMD ["/svc"]
