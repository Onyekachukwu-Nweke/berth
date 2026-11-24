# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mountabo-api ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mountabo-api /mountabo-api
ENV MOUNTABO_HTTP_ADDR=:7778
EXPOSE 7778
USER nonroot:nonroot
ENTRYPOINT ["/mountabo-api"]
