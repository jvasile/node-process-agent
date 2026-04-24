#!/bin/bash
systemctl stop node-process-agent || true
systemctl disable node-process-agent || true
