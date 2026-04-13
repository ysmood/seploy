# Overview

A simple and efficient tool to deploy docker images to a remote server via SSH.
The remote server only needs to have ssh, no need to have docker, a empty server is enough.
No need for a shared docker registry, the tool will automatically transfer the image to the remote server and run it.
Image will efficiently transferred with layer caching, so only the changed layers will be transferred.

The dependency for the tool is only docker, no need for local ssh.

Example to use it as a CLI tool:

```go
go install github.com/ysmood/seploy@latest

docker pull nginx

seploy -t admin@stg up nginx
```

Example to use it as a library:

```go
package main

import (
	"context"
	"log"

	"github.com/ysmood/seploy/pkg/seploy"
)

func main() {
	ctx := context.Background()

    // Without ssh key, it will try to use ssh-agent for authentication.
	d := seploy.Deployment{
        SSHTarget: "admin@stg",
        ImageTag:  "my-app:v0.0.1",
    }

	if err := d.Deploy(ctx); err != nil {
		panic(err)
	}
}
```
