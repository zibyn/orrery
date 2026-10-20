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
# The context has no .git (.dockerignore), so the tag arrives here instead.
ARG VERSION=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags embed -trimpath \
    -ldflags="-s -w -X github.com/zibyn/orrery/internal/buildinfo.version=${VERSION}" \
    -o /out/orrery-server ./cmd/orrery-server

FROM alpine:3.22
RUN adduser -D -u 10001 orrery
COPY --from=build /out/orrery-server /usr/local/bin/orrery-server
USER orrery
ENTRYPOINT ["orrery-server"]
CMD ["serve"]
