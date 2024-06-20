## obscurity

> [!CAUTION]
> Security through obscurity[^1] is not recommended at all.

I use HTTPS + [cloudflare workers](../obscurity/cloudflare-workers-obscurity.js) + Nginx [obscurity.conf](../obscurity/obscurity.conf.template) to hide the path.

```
location / {
    include /etc/nginx/include/obscurity.conf;

    # https://serverfault.com/a/587432
    proxy_max_temp_file_size 0;

    # ... security hardening

    proxy_redirect off;
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
}
```

[^1]: https://en.wikipedia.org/wiki/Security_through_obscurity
