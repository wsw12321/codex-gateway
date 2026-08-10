#!/bin/sh
set -eu

# A TCP probe deliberately avoids placing the internal Bearer secret in the
# healthcheck process list. Authentication and OAuth health are checked only
# by the explicit post-login smoke test; Gateway readiness currently covers
# PostgreSQL connectivity, not the sidecar or real upstream.
exec nc -z -w 3 127.0.0.1 "${CLIPROXY_PORT:-8317}"
