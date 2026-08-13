# hplc-parser (Go). Built on the arc-chem self-hosted runner (LAN access to
# zot.hwcopeland.net) and pushed to zot.hwcopeland.net/freedom/hplc-parser.
# linux/amd64 nodes. Static binary on distroless — no interpreter, no pip.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /hplc-parser .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /hplc-parser /hplc-parser
USER nonroot:nonroot
ENTRYPOINT ["/hplc-parser"]
