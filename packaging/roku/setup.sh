#!/bin/sh
set -eu
cd "$(dirname "$0")"
if [ -e .env ]; then
    echo '.env already exists; keeping its secrets and settings.'
    exit 0
fi
if [ "$#" -ne 1 ] || [ ! -d "$1" ]; then
    echo 'Usage: sh setup.sh /absolute/path/to/media' >&2
    exit 1
fi
command -v openssl >/dev/null
media_root=$(cd "$1" && pwd -P)
case "$media_root" in
    *"'"*|*"
"*) echo 'Use a media path without single quotes or newlines.' >&2; exit 1 ;;
esac
umask 077
# A hard link publishes the completed file atomically without overwriting .env.
temporary=$(mktemp .env.XXXXXXXX)
trap 'rm -f "$temporary"' EXIT HUP INT TERM
{
    printf 'POSTGRES_PASSWORD=%s\n' "$(openssl rand -hex 24)"
    printf 'SECRET_KEY=%s\n' "$(openssl rand -base64 48 | tr -d '\n')"
    printf "MEDIA_ROOT='%s'\n" "$media_root"
    printf '%s\n' 'SILO_IMAGE=ghcr.io/manava/silo-server-roku:v0.1.0-roku.9' 'PORT=8090' 'JF_PORT=8096' 'ABS_PORT=13378'
} > "$temporary"
ln "$temporary" .env
printf '%s\n' 'Created private .env. Start with: docker compose up -d'
