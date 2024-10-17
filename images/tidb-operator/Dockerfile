FROM alpine:3.14

RUN apk add tzdata bind-tools --no-cache
ADD tidb-scheduler /usr/local/bin/tidb-scheduler
ADD tidb-discovery /usr/local/bin/tidb-discovery
ADD tidb-controller-manager /usr/local/bin/tidb-controller-manager
ADD tidb-admission-webhook /usr/local/bin/tidb-admission-webhook
