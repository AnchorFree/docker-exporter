# docker-exporter
Simple exporter for docker restarts

it exports two metrics:

 * `docker_container_restart_count` wich is Gauge from `docker inspect` for each container on the host
 * `docker_container_healthy` wich is 1 if container is up and running or healthcheck is passing and 0 if anything in between

It accepts following parameters:
```
  -interval duration
    	Interval between docker scrapes (default 1m0s)
  -listen string
    	Address to listen on (default ":8080")
```

it is based on docker EnvClient, hence use corresponding parameters as stated in the documentation:
```
// Use DOCKER_HOST to set the url to the docker server.
// Use DOCKER_API_VERSION to set the version of the API to reach, leave empty for latest.
// Use DOCKER_CERT_PATH to load the tls certificates from.
// Use DOCKER_TLS_VERIFY to enable or disable TLS verification, off by default.
```

### versioning 
We follow following convention:

v0.1.0 where:

v0 - major release number, braking changes are accepted between those release versions

1  - second number is minor release number, there should be no braking chnages within it

0  - bugfix index, in case bugs were found within scope of current release, this number should identify amount of bugfix releases
