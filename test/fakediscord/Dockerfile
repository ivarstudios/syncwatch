FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/fakediscord ./test/fakediscord

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fakediscord /fakediscord
EXPOSE 8080
ENTRYPOINT ["/fakediscord"]
