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

For Kubernetes, the Helm chart's Ingress and HTTPRoute do the routing; set the controller's body size and timeout annotations as above.
