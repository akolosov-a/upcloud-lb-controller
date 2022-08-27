#!/bin/sh

INTERNAL_IP=$(curl http://169.254.169.254/metadata/v1/network/interfaces/2/ip_addresses/1/address)

export INSTALL_K3S_EXEC="server --node-ip ${INTERNAL_IP} --disable servicelb,traefik"
curl -sfL https://get.k3s.io | sh -
