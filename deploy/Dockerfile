FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/stars-sandbox-server ./cmd/stars-sandbox-server

FROM alpine:3.22
RUN adduser -D -u 10001 stars
COPY --from=build /out/stars-sandbox-server /usr/local/bin/stars-sandbox-server
USER stars
ENTRYPOINT ["stars-sandbox-server"]
CMD ["serve"]
