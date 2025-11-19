#!/usr/bin/env bash
set -e

# Script to run nginx cache proxy locally for development
# This connects to the asset server running on localhost:8090

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NGINX_CONF="${SCRIPT_DIR}/nginx-cache.conf"
CONTAINER_NAME="teranode-nginx-cache"
NGINX_PORT="${NGINX_PORT:-8001}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}Starting local nginx cache proxy...${NC}"

# Check if container is already running
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo -e "${YELLOW}Stopping existing container...${NC}"
    docker stop "${CONTAINER_NAME}" >/dev/null 2>&1 || true
    docker rm "${CONTAINER_NAME}" >/dev/null 2>&1 || true
fi

# Check if the nginx config exists
if [ ! -f "${NGINX_CONF}" ]; then
    echo -e "${RED}Error: nginx config not found at ${NGINX_CONF}${NC}"
    exit 1
fi

echo -e "${GREEN}Starting nginx container...${NC}"
echo -e "  - Nginx will be available at: ${YELLOW}http://localhost:${NGINX_PORT}${NC}"
echo -e "  - Backend asset server: ${YELLOW}localhost:8090${NC}"
echo -e "  - Cache directory: ${YELLOW}/tmp/nginx/cache${NC}"
echo ""

# Create a temporary nginx config with dynamic DNS resolver
TMP_CONF=$(mktemp)
trap "rm -f ${TMP_CONF}" EXIT

# Start a temporary container to get the DNS resolver
TEMP_CONTAINER=$(docker run -d --rm nginx:alpine sleep 10)
DNS_RESOLVER=$(docker exec "${TEMP_CONTAINER}" awk '/^nameserver/ {print $2; exit}' /etc/resolv.conf)
docker stop "${TEMP_CONTAINER}" >/dev/null 2>&1

echo -e "${GREEN}Detected DNS resolver: ${YELLOW}${DNS_RESOLVER}${NC}"

# Replace the placeholder resolver with the detected one
sed "s/resolver 0\.0\.0\.0 valid=/resolver ${DNS_RESOLVER} valid=/" "${NGINX_CONF}" > "${TMP_CONF}"

# Run nginx in Docker with host networking on Linux, or host.docker.internal on macOS
docker run -d \
    --name "${CONTAINER_NAME}" \
    -p "${NGINX_PORT}:8000" \
    -v "${TMP_CONF}:/etc/nginx/nginx.conf:ro" \
    --add-host host.docker.internal:host-gateway \
    --tmpfs /tmp/nginx:rw,exec,size=512m \
    nginx:alpine sh -c "mkdir -p /tmp/nginx/cache && nginx -g 'daemon off;'"

echo -e "${GREEN}✓ Nginx cache proxy started successfully!${NC}"
echo ""
echo -e "Test with:"
echo -e "  ${YELLOW}curl -v http://localhost:${NGINX_PORT}/api/v1/block/<block_hash>${NC}"
echo ""
echo -e "View logs:"
echo -e "  ${YELLOW}docker logs -f ${CONTAINER_NAME}${NC}"
echo ""
echo -e "Stop the proxy:"
echo -e "  ${YELLOW}docker stop ${CONTAINER_NAME}${NC}"
echo ""
