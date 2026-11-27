FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /crypta ./cmd/crypta

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /crypta /usr/local/bin/crypta
VOLUME ["/data", "/downloads"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["crypta"]
CMD ["serve"]
