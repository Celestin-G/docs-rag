# Built on the runner's own architecture and cross-compiled to the target, so
# adding arm64 costs a second build rather than a QEMU-emulated one. Go needs
# nothing but GOARCH for this; CGO is already off.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/rag .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/rag /usr/local/bin/rag
EXPOSE 8080
ENTRYPOINT ["rag"]
CMD ["serve"]
