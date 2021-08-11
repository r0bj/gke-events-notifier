FROM golang:1.16.7 as builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a --ldflags '-w -extldflags "-static"' -tags netgo -installsuffix netgo -o gke-events-notifier .


FROM scratch

COPY --from=builder /workspace/gke-events-notifier /

USER 65534:65534

ENTRYPOINT ["/gke-events-notifier"]
