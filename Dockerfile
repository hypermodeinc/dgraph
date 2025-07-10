###################### Stage I ######################
FROM golang:1.25.0 AS builder
RUN apt-get update && apt-get install -y --no-install-recommends \
    bzip2=1.0.8-5+b1 \
    git=1:2.39.5-0+deb12u2 \
    && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH=amd64
ARG TARGETOS=linux
WORKDIR /go/src/repo
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} make

###################### Stage II ######################
FROM ubuntu:24.04
LABEL maintainer="Hypermode Inc. <hello@hypermode.com>"
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates=20240203 \
    curl=8.5.0-2ubuntu10.6 \
    htop=3.3.0-4build1 \
    iputils-ping=3:20240117-1build1 \
    jq=1.7.1-3ubuntu0.24.04.1 \
    less=590-2ubuntu2.1 \
    sysstat=12.6.1-2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /go/src/repo/dgraph/dgraph /usr/local/bin/
COPY --from=builder /go/src/repo/contrib/standalone/run.sh /
RUN chmod +x /run.sh
WORKDIR /dgraph
ENV GODEBUG=madvdontneed=1
CMD ["/run.sh"]
