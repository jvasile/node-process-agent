#!/bin/bash
set -e

if ! id -u node-process-agent > /dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin \
        --groups disk node-process-agent
fi

install -d -m 755 /etc/node-process-agent

systemctl daemon-reload
systemctl enable node-process-agent
