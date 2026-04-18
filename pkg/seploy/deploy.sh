# No `set -e`: this script manages rollback explicitly and every command
# below has a deliberate policy (fail→abort, fail→rollback, or fail→ignore).
# `-u` catches unset vars, `pipefail` surfaces failures through pipelines.
set -uo pipefail

die() {
	echo "ERROR: $*" >&2
	exit 1
}

# --- Image and network prep ------------------------------------------------
# No prior state to preserve yet; any failure here aborts the deploy.

docker pull {{.registryTag}} || die "docker pull {{.registryTag}} failed"
docker tag {{.registryTag}} {{.tag}} || die "docker tag {{.registryTag}} {{.tag}} failed"
docker rmi {{.registryTag}} >/dev/null 2>&1 || true  # harmless if still referenced

if ! docker network inspect seploy >/dev/null 2>&1; then
	docker network create seploy || die "failed to create docker network 'seploy'"
fi

# --- Locate and preserve previous container --------------------------------

# The template renders {{.name}} already shell-escaped (e.g. 'nginx').
# That's correct for argv positions, but breaks docker's regex filter where
# the quotes become literal characters. Bind to a shell var so we can use
# the unquoted value via "$name" in filters.
name={{.name}}

container_id=$(docker ps -aqf "name=^${name}\$") || die "failed to list containers"

if [ -n "$container_id" ]; then
	echo "Stopping container $name ..."
	if docker stop "$name"; then
		# If run with --rm, docker removes the container asynchronously after
		# stop. Wait so the existence check below isn't racy.
		docker wait "$container_id" >/dev/null 2>&1 || true
	else
		# Stop failed — treat as unmanageable; skip preservation.
		container_id=""
	fi
fi

# If the previous container was --rm, it's gone now.
if [ -n "$container_id" ] && ! docker inspect "$container_id" >/dev/null 2>&1; then
	container_id=""
fi

# Preserve the previous container by renaming it to its ID so rollback can
# restore it. Rename can race with async `--rm` removal: inspect above may
# see the container but rename then fails because it's gone. In that case
# there's nothing to preserve — clear container_id and continue.
if [ -n "$container_id" ]; then
	echo "Renaming container $name to $container_id ..."
	if ! docker rename "$name" "$container_id" 2>/dev/null; then
		if docker inspect "$container_id" >/dev/null 2>&1; then
			die "failed to rename container $name to $container_id"
		fi
		container_id=""
	fi
fi

# --- Cleanup / rollback helpers --------------------------------------------

cleanup() {
	if [ -n "$container_id" ]; then
		echo "Cleanup previous container..."
		docker rm -f "$container_id" >/dev/null 2>&1 || true
	fi
}

rollback() {
	if [ -n "$container_id" ]; then
		echo "Rollback to previous container..."

		# Free the $name slot. The new container may be running, may be
		# mid-async-removal (--rm), or may be an already-removed leftover.
		docker stop "$name" >/dev/null 2>&1 || true
		docker wait "$name" >/dev/null 2>&1 || true
		docker rm -f "$name" >/dev/null 2>&1 || true

		# Critical steps — if either fails we're in a broken state and must
		# surface it loudly rather than exit 1 silently.
		docker rename "$container_id" "$name" \
			|| die "rollback failed: could not rename $container_id back to $name (manual intervention required)"
		docker start "$name" \
			|| die "rollback failed: could not start $name (manual intervention required)"

		echo "Rolled back to previous container"
	fi

	exit 1
}

# --- Start the new container -----------------------------------------------

echo "Starting container $name ..."

if ! docker run {{.service}} \
	--name {{.name}} \
	--label {{.hostLabel}} \
	--label {{.repoLabel}} \
	--label {{.refLabel}} \
	-e {{.host}} \
	--env-file <(echo {{.env}} | base64 -d) \
	--log-opt max-size=300m --log-opt max-file=3 \
	{{.volumes}} {{.options}} {{.tag}} {{.commands}}
then
	rollback
fi

{{ if eq .notService "true" }}
	cleanup
	exit 0
{{ end}}

# --- Stability check -------------------------------------------------------

echo "Waiting for container to be stable..."
sleep 5

restartCount=$(docker inspect -f {{"'{{.RestartCount}}'"}} "$name" | tr -d "'")
restartCount=${restartCount:-0}

if [ "$restartCount" -gt 0 ]; then
	echo "Stopping container $name due to restart count: $restartCount, logs for the container:"

	docker logs -n 1000 "$name" || true
	docker stop "$name" >/dev/null 2>&1 || true
	docker wait "$name" >/dev/null 2>&1 || true
	docker rm -f "$name" >/dev/null 2>&1 || true

	rollback
else
	cleanup
fi

echo "Container $name started"
