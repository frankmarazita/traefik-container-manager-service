# traefik-container-manager-service

Wakes stopped containers on demand and stops them again once they go idle. It
works in combination with the `traefik-container-manager` middleware, which
calls this API when a request arrives for a service that is not running.

The service listens on port `10000`.

## API

A single endpoint under `/api/`, driven by query parameters:

| Parameter | Required | Meaning                                                    |
| --------- | -------- | ---------------------------------------------------------- |
| `name`    | yes      | Service name to match against the container labels          |
| `timeout` | yes      | Idle seconds before the containers are stopped              |
| `host`    | no       | Request host, used when matching by label rather than name  |
| `path`    | no       | Request path, used when matching by label rather than name  |

`timeout` must be a positive integer. The value is recorded the first time a
service name is seen and reused for that service afterwards.

## Labels

Containers opt in with `traefik-container-manager.name` (mandatory), and
optionally `traefik-container-manager.host` and `traefik-container-manager.path`.

The name label can be added to every container that should be stopped alongside
the service the middleware is defined on.

A container is selected when any of the following matches, checked in order:

1. `name` equals the `traefik-container-manager.name` label, ignoring case.
2. `host` and the `traefik-container-manager.host` label are both non-empty and
   either is a prefix of the other.
3. `path` and the `traefik-container-manager.path` label are both non-empty and
   either is a prefix of the other.

Host and path are only consulted when the request supplies them. A request that
sends just a name never matches another service's host or path labels.

## Configuring through labels

The name `generic-container-manager` is reserved for the case where the traefik
config comes from labels rather than dynamic config. Requests using that name
are resolved through the `host` or `path` label instead, matched against the
HTTP request that is trying to wake the container — traefik's `Host` rule or
`PathPrefix` rule on the router. Use this manager container itself to create
that middleware.

Reference labels:

```yaml
labels:
  - traefik.enable=true
  - traefik.http.routers.manager.entrypoints=entryhttp
  - traefik.http.routers.manager.rule=HostRegexp(`{host:.+}`)
  - traefik.http.routers.manager.priority=1
  - traefik.http.middlewares.manager.errors.status=404
  - traefik.http.middlewares.manager.errors.service=manager
  - traefik.http.middlewares.manager.errors.query=/
  - traefik.http.routers.manager.middlewares=manager-starter
  - traefik.http.services.manager.loadbalancer.server.port=10000
  - traefik.http.middlewares.manager-starter.plugin.traefik-container-manager.name=generic-container-manager
```

The router rule syntax above is for traefik v2. On v3 use ``HostRegexp(`.+`)``.

## Security

This service needs access to the Docker socket to start and stop containers,
which is equivalent to root on the host. The API has no authentication, so
anyone who can reach it can start and stop any labelled container.

Keep it on an internal network and do not expose it publicly. Rather than
mounting `/var/run/docker.sock` directly, prefer a socket proxy restricted to
container list, start and stop.
