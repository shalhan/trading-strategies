FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /paper ./cmd/paper

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /paper /usr/local/bin/paper
VOLUME /state
EXPOSE 8899
ENTRYPOINT ["paper", "-state", "/state", "-datadir", "/state/data", "-addr", ":8899"]
