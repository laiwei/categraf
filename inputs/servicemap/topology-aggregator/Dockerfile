FROM golang:1.22-alpine AS builder
ARG BINARY=topology-aggregator
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/${BINARY} .

FROM alpine:3.19
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /out/topology-aggregator /usr/local/bin/topology-aggregator
EXPOSE 9098
ENTRYPOINT ["/usr/local/bin/topology-aggregator"]
