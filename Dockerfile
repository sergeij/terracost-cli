FROM --platform=$BUILDPLATFORM golang:1.25.10-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /terracost-cli \
    .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /terracost-cli /usr/local/bin/terracost-cli

ENTRYPOINT ["/usr/local/bin/terracost-cli"]
