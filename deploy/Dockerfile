FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/spendbot ./cmd/spendbot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/spendbot /spendbot
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/spendbot", "healthcheck"]
ENTRYPOINT ["/spendbot"]
