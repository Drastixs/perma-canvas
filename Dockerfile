FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /perma-canvas ./cmd/perma-canvas
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /perma-canvas /perma-canvas
ENV LISTEN_ADDR=0.0.0.0:8080 DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/perma-canvas"]
