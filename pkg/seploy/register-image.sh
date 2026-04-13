set -e

docker tag $tag $registry_tag
docker push $registry_tag
docker rmi $registry_tag
