# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/generic-mail ./cmd/generic-mail

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/generic-mail /generic-mail
USER nonroot:nonroot
EXPOSE 993 465 587 8080
ENTRYPOINT ["/generic-mail"]
