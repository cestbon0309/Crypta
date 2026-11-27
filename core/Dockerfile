FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /crypta ./cmd/crypta
# pikpakcli fetches from PikPak for pikpak:/path jobs.
ARG TARGETARCH
RUN case "${TARGETARCH:-$(go env GOARCH)}" in \
      amd64) f=x86_64 sum=91bb9407a239c587aeb7a52b2bbf0bfe1319e44d3c9e6a8b278b5f5423566073 ;; \
      arm64) f=arm64 sum=172bc7c962cfdb60e82dd81741236b6e32c5b2720c39ee3afadd5eaf2ab37c87 ;; \
      *) echo "no pikpakcli build for this architecture" >&2; exit 1 ;; \
    esac \
 && wget -qO /tmp/pikpakcli.tar.gz "https://github.com/52funny/pikpakcli/releases/download/v0.23/pikpakcli_Linux_$f.tar.gz" \
 && echo "$sum  /tmp/pikpakcli.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/pikpakcli.tar.gz -C / pikpakcli

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /crypta /pikpakcli /usr/local/bin/
VOLUME ["/data", "/downloads"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["crypta"]
CMD ["serve"]
