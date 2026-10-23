FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/mock-model ./cmd/mock-model

FROM alpine:3.22
RUN adduser -D -H app
COPY --from=build /out/gateway /usr/local/bin/gateway
COPY --from=build /out/mock-model /usr/local/bin/mock-model
USER app
EXPOSE 8080
ENTRYPOINT ["gateway"]
