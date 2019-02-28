#!/bin/bash -xe

sudo iptables -I INPUT -i docker0 -j ACCEPT

#docker run -d -p 3000:3000 --name=grafana -e "GF_SERVER_ROOT_URL=http://localhost" -e "GF_SECURITY_ADMIN_PASSWORD=secret" grafana/grafana
docker run -p 9090:9090 --name=prometheus -v $PWD/test/prometheus.yml:/etc/prometheus/prometheus.yml prom/prometheus
