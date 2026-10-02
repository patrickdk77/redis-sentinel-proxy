redis-sentinel-proxy
====================

Small command utility that:

* Given a redis sentinel server listening on `SENTINEL_PORT`, keeps asking it for the address of a master named `NAME`

* Proxies all tcp requests that it receives on `PORT` to that master

Usage:

`./redis-sentinel-proxy -listen IP:PORT -sentinel :SENTINEL_PORT -master NAME`

## Usage


### 1. Envioment variables or commandline arguments

Environment Variables     | Description                                       | Required | Default
------------------------- | ------------------------------------------------- | -------- | -----------------
LISTEN                    | IP and Port to bind the proxy to                  |          | :9999
SENTINEL                  | sentinel server and port to connect to, or list   |          | :26379
MASTER                    | master group name                                 |          | mymaster
USERNAME                  | username to authenticate with if using v6 acls    |          | -
PASSWORD                  | password to authenticate with to sentinel         |          | -
REDIS_PASSWORD            | password to authenticate with to redis if different from sentinel |          | -
DEBUG                     | debug output                                      |          | false
TIMEOUTMS                 | timeout for sentinel and master connections, must be greater than 0 |          | 2000
CHECKMS                   | poll time to check sentinel for master changes. 0 turns polling off and needs EVENTLISTENER. The proxy refuses to start on a negative value |          | 250
EVENTLISTENER             | subscribe to master changes from sentinal         |          | false
MAJORITY                  | switch only when most sentinels that answer agree on the master. Each sentinel counts once, by its run ID. Also asks up to 10 peer sentinels that most of the listed sentinels report. A switch-master event starts a recount instead of switching |          | false
MAXCONNS                  | maximum client connections, 0 for no limit. The proxy closes connections over the limit |          | 10000
SENTINELTLS               | connect to sentinels over TLS                     |          | false
SENTINELTLSCA             | CA file to verify sentinel certificates. Without it the proxy uses the system CA roots |          | -
SENTINELTLSCERT           | client certificate file, for sentinels that require one |          | -
SENTINELTLSKEY            | key file for SENTINELTLSCERT                      |          | -
SENTINELTLSSERVERNAME     | name to check sentinel certificates against, when they don't cover the address in SENTINEL |          | -

Boolean settings take true or false. 1, 0, t and f also work, on/off and yes/no don't, and the proxy exits at startup on any other value.



### 2. Running the proxy

Edit `kubernetes/redis-sentinel-proxy-deployment.yaml`:

```bash
vim kubernetes/redis-sentinel-proxy-deployment.yaml
...
        args:
          - "-master"
          - "primary"
          - "-sentinel"
          - "redis-sentinel.$(NAMESPACE):26379" # change this to the sentinel address
```

Create `redis-sentinel-proxy-deployment` that uses `redis-sentinel-proxy`:

```bash
kubectl apply -f kubernetes/redis-sentinel-proxy-deployment.yaml
deployment "redis-sentinel-proxy" configured
```

Check if deployment is running: 

```bash
kubectl get pods
redis-sentinel-proxy-2064359825-s4n0k   1/1       Running   0          1d
```

Expose `redis-sentinel-proxy-deployment`:

```bash
kubectl apply -f kubernetes/redis-sentinel-proxy-service.yaml
```

