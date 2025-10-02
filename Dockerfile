FROM alpine AS depend
RUN apk add --update --no-cache ca-certificates tzdata

FROM busybox:stable-musl

ARG TARGETOS
ARG TARGETARCH

COPY --from=depend /etc/ssl/certs /etc/ssl/certs
COPY --from=depend /usr/share/zoneinfo /usr/share/zoneinfo
COPY ./script/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

WORKDIR /dashboard
COPY dist/dashboard-${TARGETOS}-${TARGETARCH} ./app

VOLUME ["/dashboard/data"]
EXPOSE 8008
ARG TZ=Asia/Shanghai
ENV TZ=$TZ
ENTRYPOINT ["/entrypoint.sh"]
