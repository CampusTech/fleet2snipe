FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/fleet2snipe .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/fleet2snipe /usr/local/bin/fleet2snipe
USER nonroot
EXPOSE 9090
ENTRYPOINT ["fleet2snipe"]
CMD ["serve"]
