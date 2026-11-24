# syntax=docker/dockerfile:1
# Multi-stage build: compile in a full Go image, ship a small runtime image.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/networkai ./cmd/networkai

FROM alpine:3.22
# CA certificates come from the build stage, so this stage needs no network.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN adduser -D -u 10001 app && mkdir -p /data/media && chown app /data/media
USER app
COPY --from=build /out/networkai /usr/local/bin/networkai
ENV MEDIA_DIR=/data/media
EXPOSE 8080
ENTRYPOINT ["networkai"]
CMD ["api"]
