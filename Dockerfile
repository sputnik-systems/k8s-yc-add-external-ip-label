FROM golang:1.17-stretch AS build
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64 
WORKDIR /build
RUN apt update \
    && apt install -y git
COPY go.mod go.sum ./
RUN go mod download -x
COPY . .
RUN go build -v -a -installsuffix cgo -o external-ip-label-updater \
    -ldflags "-X main.gitTag=`git describe --tags HEAD` -X main.gitCommitId=`git rev-parse --short HEAD`" \
    cmd/external-ip-label-updater/main.go

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /build/external-ip-label-updater /usr/local/bin/external-ip-label-updater
ENTRYPOINT ["/usr/local/bin/external-ip-label-updater"]
