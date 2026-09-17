# Multi-stage build; distroless runtime. --provenance=false is applied by the
# CI build job (single-platform manifest; registry dropped acked manifests
# under attestation surfaces).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
<<<<<<< HEAD
=======
# All package sources (main.go + gitlab.go + github.go) and their tests.
>>>>>>> 879512d (fix: copy all package sources in Dockerfile build stage)
COPY *.go ./
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ci-exporter .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ci-exporter /ci-exporter
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/ci-exporter"]
