# portproxy

A shitty TCP proxy that relays all requests to a local port to a remote server.

```
portproxy -port 8080 -raddr google.com:80
```

Will proxy all TCP requests on `localhost:8080` and send them to `google.com`.


## Highjacking HTTP

If the `http` flag is given:

```
portproxy -http -port 8080 -raddr google.com:80
```

The proxy will attempt to detect HTTP requests. If a HTTP request contains an
 `Origin` header, the response from the remote server will be modified to
  include a `Access-Control-Allow-Origin: *` header.

## Motivation

The sole purpose of this proxy was to bypass a remote API that didn't do CORS.
