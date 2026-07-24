FROM docker.io/library/alpine:3.22
RUN apk add --no-cache chrony
# chronyd needs its runtime dir for the pidfile and command socket. The OpenRC
# service normally creates it, but we run chronyd directly, and cap_drop:ALL
# leaves it unable to create/chown the dir itself - so pre-create it owned by
# the chrony user.
RUN mkdir -p /run/chrony && chown chrony:chrony /run/chrony
CMD ["chronyd", "-d", "-f", "/etc/chrony/chrony.conf"]
