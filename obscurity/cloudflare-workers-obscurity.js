addEventListener('fetch', event => {
    event.respondWith(handleRequest(event.request))
})

async function handleRequest(request) {
    // An error is thrown that immutable headers can’t be modified
    request = new Request(request);

    let url = new URL(request.url);
    if (url.pathname.indexOf('/' + BW_OBS) !== 0) {
        return new Response('Hello world')
    }

    request.headers.set('bw', BW_HEADER)

    return fetch('http://' + BW_HOST +
        url.pathname.replace(new RegExp(`^\/${BW_OBS}`), ''),
        {
            method: request.method,
            body: request.body,
            headers: request.headers,
            keepalive: request.keepalive,
        }
    );
}
