FROM golang as builder
WORKDIR $GOPATH/src/github.com/drdaeman/expiring-telegram
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags "-s -w -linkmode external -extldflags '-fno-PIC -static'" -o /go/bin/janitor .
RUN mkdir /tmp/empty \
 && touch /tmp/empty/.keep \
 && chown -R nobody:nogroup /tmp/empty \
 && chmod 0444 /tmp/empty/.keep \
 && chmod 0755 /tmp/empty

FROM scratch
LABEL maintainer="Aleksei Zhukov <me@zhukov.al>"
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder /go/bin/janitor /bin/janitor
COPY --chown=nobody:nogroup --from=builder /tmp/empty /data
USER nobody:nogroup
VOLUME /data
ENV JANITOR_DB=sqlite:/data/janitor.sqlite3
ENV JANITOR_INI_PATH=/etc/janitor.ini
ENTRYPOINT ["/bin/janitor"]
