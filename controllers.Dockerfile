# One image with both controllers (the Deployments pick the binary): the reverter builds from the root module, the crash detector from its own.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY reverter/ ./reverter/
COPY pathmatch/ ./pathmatch/
RUN CGO_ENABLED=0 go build -o /out/reverter ./reverter

COPY crash-detector/ ./crash-detector/
RUN cd crash-detector && CGO_ENABLED=0 go build -o /out/crash-detector .

FROM alpine:3.20
COPY --from=build /out/reverter /out/crash-detector /
COPY reverter/catalog.json /catalog.json
