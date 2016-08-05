FROM alpine

RUN apk update && apk add ca-certificates && mkdir /srv/http

COPY index.html /srv/http/index.html
COPY putd /usr/local/bin/putd

USER 1

EXPOSE 8080

CMD putd -listen :8080
