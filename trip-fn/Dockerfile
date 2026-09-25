# Build context is the repo root (docker build -f trip-fn/Dockerfile .); one image carries both extended variants, the manifests pick one via command.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY interceptor ./interceptor
COPY pathmatch ./pathmatch
RUN CGO_ENABLED=0 go build -o /out/interceptor ./interceptor

FROM node:22-alpine
WORKDIR /app
COPY trip-fn/vanilla-extended.js trip-fn/saga-extended.js ./
COPY interceptor/shim.js /atomic/shim.js
COPY reverter/catalog.json /atomic/catalog.json
COPY --from=build /out/interceptor /interceptor
ENV NODE_ENV=production
ENTRYPOINT ["/interceptor", "node", "/app/vanilla-extended.js"]
