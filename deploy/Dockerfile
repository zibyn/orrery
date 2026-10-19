# The Web and Go stages run on the build platform and cross-compile, so a
# multi-arch build emulates only the final stage (ADR-0023).
FROM --platform=$BUILDPLATFORM node:24-alpine AS web
WORKDIR /web
RUN npm install -g pnpm@12.4.2
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags embed -trimpath -ldflags="-s -w" -o /out/stars-sandbox-server ./cmd/stars-sandbox-server

FROM alpine:3.22
RUN adduser -D -u 10001 stars
COPY --from=build /out/stars-sandbox-server /usr/local/bin/stars-sandbox-server
USER stars
ENTRYPOINT ["stars-sandbox-server"]
CMD ["serve"]
