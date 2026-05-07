#!/bin/bash
# Ensure config directory exists and start easy_proxies

mkdir -p data logs

docker compose pull && docker compose down && docker compose up -d
