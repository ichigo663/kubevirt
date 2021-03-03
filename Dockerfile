FROM golang:1.13.14 as builder

RUN wget https://github.com/go-delve/delve/archive/v1.3.2.tar.gz && \
    tar xf v1.3.2.tar.gz && \
    cd delve-1.3.2/cmd/dlv && \
    go install

FROM gcr.io/distroless/base
COPY --from=builder /go/bin/dlv /usr/bin/dlv
