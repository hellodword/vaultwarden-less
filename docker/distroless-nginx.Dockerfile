# https://github.com/kyos0109/nginx-distroless/blob/4fa36b8c066303f34e490aad7b407d447ade4b7d/Dockerfile
FROM nginx as base

# https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
ARG TIME_ZONE

# https://github.com/GoogleContainerTools/distroless/blob/64ac73c84c72528d574413fb246161e4d7d32248/common/variables.bzl#L18
ENV USER=nonroot
ENV UID=65532

RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid "${UID}" \
    "${USER}"

RUN sed -i -E 's/(listen[ ]+)80;/\18080;/' /etc/nginx/conf.d/default.conf

RUN mkdir -p /opt/bin /opt/etc /opt/usr/bin /opt/var/cache/nginx && \
    cp /usr/share/zoneinfo/${TIME_ZONE:-UTC} /opt/etc/localtime && \
    cp -a --parents /etc/passwd /opt && \
    cp -a --parents /etc/group /opt && \
    cp -aL --parents /var/run /opt && \
    cp -a --parents /usr/lib/nginx /opt && \
    cp -a --parents /usr/share/nginx /opt && \
    cp -a --parents /var/log/nginx /opt && \
    cp -a --parents /etc/nginx /opt && \
    cp -a --parents "$(which nginx)" /opt && \
    ldd "$(which nginx)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
    cp -a --parents "$(which nginx-debug)" /opt && \
    ldd "$(which nginx-debug)" | grep -oP '(?<==> )/lib/[^ ]+\.so' | xargs -I {} bash -xc 'cp -a --parents {}* /opt' && \
    true

RUN chown -R nonroot:nonroot /opt/var/cache /opt/var/run

RUN touch /opt/var/run/nginx.pid && \
    chown -R nonroot:nonroot /opt/var/run/nginx.pid

FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=base /opt /

EXPOSE 8080 8443

ENTRYPOINT ["/usr/sbin/nginx", "-g", "daemon off;"]
