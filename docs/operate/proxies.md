---
description: "Put Hangar behind a reverse proxy for TLS: the settings every proxy needs, Helm values, and large file uploads through nginx."
---

# TLS and reverse proxies

Hangar serves plain HTTP; put a reverse proxy in front of it for TLS. Whatever proxy you use:

- Set `server.public_url` to the address clients use, with `https://`.
- Set `server.trusted_proxies` to the proxy's addresses, so Hangar believes its `X-Forwarded-For` and `X-Forwarded-Proto` headers and nobody else's.
- Do not buffer request or response bodies, and do not set a total request timeout: uploads and downloads stream and can take hours. Hangar closes connections idle for 5 minutes itself.
- Allow large request bodies (5 TiB files go up in parts of up to 512 MiB).
- Route `datasets-server.<host>` to Hangar as well, for the `datasets` library (see [Datasets](../guides/datasets.md#the-datasets-server-name)).
- Ollama needs a certificate it trusts, or `--insecure`.

=== "Traefik"

    ```yaml
    # Docker labels on the Hangar container
    traefik.enable: "true"
    traefik.http.routers.hangar.rule: Host(`hangar.example.com`) || Host(`datasets-server.hangar.example.com`)
    traefik.http.routers.hangar.entrypoints: websecure
    traefik.http.routers.hangar.tls.certresolver: letsencrypt
    traefik.http.services.hangar.loadbalancer.server.port: "8080"
    ```

    Traefik streams bodies by default. Set `respondingTimeouts.readTimeout` and `writeTimeout` to `0` on the entry point, as the scaled Compose example does.

=== "nginx"

    ```nginx
    server {
        listen 443 ssl;
        http2 on;
        server_name hangar.example.com datasets-server.hangar.example.com;
        ssl_certificate     /etc/ssl/hangar.crt;
        ssl_certificate_key /etc/ssl/hangar.key;

        client_max_body_size 0;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_max_temp_file_size 0;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;

        location / {
            proxy_pass http://127.0.0.1:8080;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
        }
    }
    ```

=== "Caddy"

    ```caddyfile
    hangar.example.com, datasets-server.hangar.example.com {
        reverse_proxy 127.0.0.1:8080 {
            flush_interval -1
            transport http {
                read_timeout 0
                write_timeout 0
            }
        }
    }
    ```

    Caddy gets and renews certificates itself, and streams request bodies by default.

=== "Cloudflare Tunnel"

    ```yaml
    # cloudflared config.yml
    tunnel: <tunnel ID>
    credentials-file: /etc/cloudflared/<tunnel ID>.json
    ingress:
      - hostname: hangar.example.com
        service: http://hangar:8080
      - hostname: datasets-server.hangar.example.com
        service: http://hangar:8080
      - service: http_status:404
    ```

    Cloudflare limits request bodies to 100 MB on the Free and Pro plans. Large files still upload, because they go up in parts of 16 MiB or more, each its own request, but a git push sends its pack in one request, so a push with more than 100 MB of non-LFS data fails through the tunnel; push it over your LAN or a direct HTTPS address. Cloudflare Access in front of Hangar also blocks clients that cannot sign in through a browser, such as `huggingface_hub`, git and Ollama, unless you add service tokens or bypass rules for them.

For Kubernetes, the Helm chart's Ingress and HTTPRoute do the routing; set the controller's body size, buffering and timeout options as above. With the ingress-nginx controller:

```yaml
# Helm values
ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-max-temp-file-size: "0"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

## Large files through nginx

nginx's defaults break large transfers, both in front of Hangar and in front of an S3-compatible store such as MinIO. Every nginx in the path, including Nginx Proxy Manager, needs these settings:

| Setting | Why |
|---|---|
| `proxy_buffering off` | Streams responses to the client as they arrive, instead of buffering them. |
| `proxy_max_temp_file_size 0` | Stops nginx spooling a response to disk when the client reads more slowly than the upstream sends. |
| `proxy_request_buffering off` | Streams uploads to the upstream instead of storing the whole body first. |
| `client_max_body_size 0` | Removes the request size limit (1 MiB by default), which rejects upload parts with `413`. |
| `proxy_read_timeout 1h`, `proxy_send_timeout 1h` | Keeps a slow transfer open; the default of 60 seconds cuts a stalled read or a slow upload part. |

With buffering on, nginx writes a response it cannot pass on fast enough to a temporary file, up to `proxy_max_temp_file_size`, which is 1024 MiB by default. When the file is full, nginx stops reading from the upstream until the client catches up. If the upstream gives up waiting first, it closes the connection, and the client gets a body that ends early. Hangar reads whole objects back from the store when it verifies an upload, so a large file whose read was cut short after about 1 GiB fails with an error such as `upload part 65/66: unexpected EOF` (64 parts of 16 MiB are 1 GiB), and the client uploads it again. `proxy_buffering off` and `proxy_max_temp_file_size 0` prevent that. Hangar resumes a read that was cut short from where it stopped, so uploads still complete, but each cut costs another request. To check a proxy, run `hangar admin doctor --large-read` (see [Run the doctor](troubleshooting.md#run-the-doctor)).

### MinIO behind nginx

When clients or Hangar reach MinIO through nginx, for example on `https://minio.example.com` to get a trusted certificate, give the MinIO server block the same settings:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name minio.example.com;
    ssl_certificate     /etc/ssl/minio.crt;
    ssl_certificate_key /etc/ssl/minio.key;

    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_buffering off;
    proxy_max_temp_file_size 0;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://127.0.0.1:9000;
        proxy_http_version 1.1;
        # Presigned URLs and request signatures cover the Host header, so
        # pass it through unchanged.
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
    }
}
```

In Nginx Proxy Manager, open the proxy host, select **Advanced** and add the directives from the table, one per line, ending each with a semicolon. Set `storage.s3.endpoint` to the nginx address and `region` to the region MinIO is configured with (see [Storage backends](storage.md#s3-compatible-stores)).
