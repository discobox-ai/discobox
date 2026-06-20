# Sessions Client Design

The client package hides Unix-socket HTTP details from the CLI. JSON endpoints
use a normal `http.Client` with a Unix dialer. Attach uses a raw Unix connection
so the caller can read and write framed PTY stream messages after the HTTP
upgrade response.
