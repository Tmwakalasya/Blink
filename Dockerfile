# Cloud Build uses this when deploy/cloudrun.sh ships Blink to Cloud Run.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /blink .

FROM gcr.io/distroless/static-debian12
COPY --from=build /blink /blink
ENTRYPOINT ["/blink"]
