FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY web ./web
COPY verification/tick-live-observations.json ./verification/tick-live-observations.json
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/rsdw-c2 .

FROM alpine:3.22

ARG HELM_VERSION=v4.2.2
ARG KUBECTL_VERSION=v1.36.2
RUN apk add --no-cache ca-certificates curl \
  && curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" | tar -xz -C /tmp \
  && mv /tmp/linux-amd64/helm /usr/local/bin/helm \
  && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
  && chmod +x /usr/local/bin/kubectl \
  && rm -rf /tmp/linux-amd64

RUN addgroup -S rsdw -g 1000 && adduser -S rsdw -u 1000 -G rsdw
COPY --from=build /out/rsdw-c2 /usr/local/bin/rsdw-c2
RUN mkdir -p /var/lib/rsdw-c2 && chown -R rsdw:rsdw /var/lib/rsdw-c2
USER 1000:1000
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/rsdw-c2"]
