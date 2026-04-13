set -e

docker pull {{.registryTag}}
docker tag {{.registryTag}} {{.tag}}
docker rmi {{.registryTag}}

# Create network if not exists
docker network inspect seploy >/dev/null 2>&1 || docker network create seploy

container_id=$(docker ps -aqf "name=^$(echo {{.name}})\$")

if [ -n "$container_id" ]
then
	echo "Stopping container {{.name}} ..."
	docker stop {{.name}} || container_id=""
fi

# If run with --rm, the container is removed on stop, so container_id is empty
if [ -n "$container_id" ] && [ -z "$(docker ps -aqf "id=$container_id")" ]; then
	container_id=""
fi

# Stop the current container if it exists, preserve the container by renaming it to $container_id
if [ -n "$container_id" ]
then
	echo "Renaming container {{.name}} to $container_id ..."
	docker rename {{.name}} "$container_id" || container_id=""
fi

cleanup()
{
	if [ -n "$container_id" ]
	then
		echo "Cleanup previous container..."
		docker rm "$container_id"
	fi
}

rollback()
{
	if [ -n "$container_id" ]
	then
		echo "Rollback to previous container..."

		docker rename "$container_id" {{.name}}
		docker start {{.name}}

		echo "Rolled back to previous container"
	fi

	exit 1
}

echo "Starting container {{.name}} ..."

# If the container starts successfully, remove the previous container.
# If the container fails to start, remove the new container and rename the previous container back to the original name.
# This is to ensure that the previous container is still running if the new container fails to start.
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

echo "Waiting for container to be stable..."

sleep 5

restartCount=$(docker inspect -f {{"'{{.RestartCount}}'"}} {{.name}})

if [ $restartCount -gt 0 ]; then
	docker stop {{.name}}

	echo "Stopped container {{.name}} due to restart count: $restartCount, logs for the container:"

	docker logs -n 1000 {{.name}}
	docker rm {{.name}}

	rollback
else
	cleanup
fi

echo "Container {{.name}} started"
