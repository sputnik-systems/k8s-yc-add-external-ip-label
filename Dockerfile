FROM golang:1.14-stretch AS build
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64 
WORKDIR /build
COPY . .
RUN apt update \
    && apt install -y git
RUN go get -v ./...
RUN export GIT_TAG=`git describe --tags HEAD` GIT_SHA_SHORT=`git rev-parse --short HEAD` \
    && go build -v -a -installsuffix cgo \
                -o k8s-yc-add-external-ip-label \
                -ldflags "-X main.version=${GIT_TAG} -X main.commitID=${GIT_SHA_SHORT}" .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /build/k8s-yc-add-external-ip-label /usr/local/bin/k8s-yc-add-external-ip-label
ENTRYPOINT ["/usr/local/bin/k8s-yc-add-external-ip-label"]
