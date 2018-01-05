FROM golang:1.9

COPY . /go/src/github.com/anchorfree/docker-exporter
RUN curl https://glide.sh/get | sh \
    && cd /go/src/github.com/anchorfree/docker-exporter \
    && glide install -v
RUN cd /go/src/github.com/anchorfree/docker-exporter \
    && CGO_ENABLED=0 go build -o /build/docker-exporter  *.go

FROM alpine

COPY --from=0 /build/docker-exporter /docker-exporter

EXPOSE 8080

ENTRYPOINT ["/docker-exporter"]
