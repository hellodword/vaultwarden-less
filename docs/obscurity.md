## obscurity

> [!CAUTION]
> Security through obscurity[^1] is not recommended at all.

I use HTTPS, Cloudflare, and Nginx to hide the public entry path.

1. Run vaultwarden-less on a local address, for example `127.0.0.1:8080`.
2. Use a long random string as the secret path segment.
3. In Cloudflare, create a proxied DNS record for `bar.foo.com` that points to the server, and force HTTPS.
4. On the server firewall, allow access to Nginx only from Cloudflare IP ranges[^2].
5. In Nginx, proxy `https://bar.foo.com/<secret>/*` to `http://127.0.0.1:8080/*`.

[^1]: https://en.wikipedia.org/wiki/Security_through_obscurity

[^2]: https://www.cloudflare.com/ips/
