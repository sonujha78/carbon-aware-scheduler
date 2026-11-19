FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY mock-api/ ./mock-api/
RUN CGO_ENABLED=0 go build -o /out/mock-api ./mock-api

FROM alpine:3.20
COPY --from=build /out/mock-api /usr/local/bin/mock-api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/mock-api"]
