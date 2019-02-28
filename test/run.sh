#!/bin/bash -xe

#docker run -d -p 3000:3000 --name=grafana -e "GF_SERVER_ROOT_URL=http://localhost" -e "GF_SECURITY_ADMIN_PASSWORD=secret" grafana/grafana
docker run --name=prometheus --network host -v $PWD/test/prometheus.yml:/etc/prometheus/prometheus.yml prom/prometheus
