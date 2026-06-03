# syntax=docker/dockerfile:1
# Builds a runnable kine image from local source for the e2e suite. The build
# context is the repository root. Produces a musl-static binary so it runs on a
# minimal Alpine runtime and so the krb5 provider's user.Current() reads
# /etc/passwd directly (no glibc NSS surprises).
FROM golang:1.25-alpine3.22 AS build
RUN apk add --no-cache gcc musl-dev git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_CFLAGS="-DSQLITE_ENABLE_DBSTAT_VTAB=1 -DSQLITE_USE_ALLOCA=1" CGO_ENABLED=1 \
    go build -tags nats -ldflags "-extldflags -static -s" -o /kine .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /kine /bin/kine
ENTRYPOINT ["/bin/kine"]
