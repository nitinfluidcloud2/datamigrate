FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /datamigrate ./cmd/datamigrate

FROM alpine:3.20
RUN apk add --no-cache open-iscsi qemu-img ca-certificates bash \
    && mkdir -p /etc/iscsi /tmp/datamigrate /run/lock/iscsi \
    && echo "InitiatorName=$(iscsi-iname)" > /etc/iscsi/initiatorname.iscsi
COPY --from=builder /datamigrate /usr/local/bin/datamigrate
RUN printf '#!/bin/sh\nmkdir -p /run/lock/iscsi\niscsid 2>/dev/null &\nsleep 1\nexec datamigrate "$@"\n' > /entrypoint.sh && chmod +x /entrypoint.sh
WORKDIR /migration
ENTRYPOINT ["/entrypoint.sh"]
