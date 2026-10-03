# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nextpage .

FROM alpine:3.22
# bash and git are used by the Bash tool and hooks; ca-certificates for HTTPS.
RUN apk add --no-cache ca-certificates bash git tzdata \
 && mkdir -p /data /courses && chmod 777 /data /courses
COPY --from=build /out/nextpage /usr/local/bin/nextpage
# HOME holds ~/.nextpage (settings, sessions, logs); /courses holds course notes.
ENV HOME=/data \
    NEXTPAGE_COURSES_DIR=/courses \
    NEXTPAGE_PROFILE=learn
WORKDIR /data
VOLUME ["/data", "/courses"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["nextpage", "--web", "--addr", ":8080"]
