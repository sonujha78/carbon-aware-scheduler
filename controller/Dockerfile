FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY controller/ ./controller/
RUN CGO_ENABLED=0 go build -o /out/controller ./controller

FROM alpine:3.20
COPY --from=build /out/controller /usr/local/bin/controller
USER 65532:65532
EXPOSE 9095
ENTRYPOINT ["/usr/local/bin/controller"]
