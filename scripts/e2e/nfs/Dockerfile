FROM alpine:3.22
RUN apk add --no-cache nfs-utils
COPY start.sh /start.sh
ENTRYPOINT ["/start.sh"]
