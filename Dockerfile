FROM alpine
COPY e-dnevnik-bot /
ENTRYPOINT ["/e-dnevnik-bot"]
