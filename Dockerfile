FROM alpine:3.24.3
COPY e-dnevnik-bot /
ENTRYPOINT ["/e-dnevnik-bot"]
