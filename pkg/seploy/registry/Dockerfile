# ysmood/seploy-registry: upstream docker registry with a heartbeat watchdog.
# seploy bind-mounts a file into the container and writes the current unix
# timestamp into it periodically. If the file hasn't been refreshed within
# SEPLOY_HEARTBEAT_TIMEOUT seconds, the watchdog kills PID 1 so the
# container never outlives a SIGKILL'd or crashed seploy process.
FROM registry:2

COPY entrypoint.sh /seploy-entrypoint.sh
RUN chmod +x /seploy-entrypoint.sh

ENV SEPLOY_HEARTBEAT_TIMEOUT=1
ENV SEPLOY_HEARTBEAT_FILE=/tmp/seploy-heartbeat

ENTRYPOINT ["/seploy-entrypoint.sh"]
CMD ["/etc/docker/registry/config.yml"]
