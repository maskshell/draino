FROM --platform=$BUILDPLATFORM golang:1.25-alpine3.21 AS build

ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /draino ./cmd/draino

FROM alpine:3.21

RUN apk add --no-cache ca-certificates
RUN addgroup -S user -g 101 && adduser -S user -u 100 -G user
USER user
COPY --from=build /draino /draino
