#!/bin/bash

parallel-ssh -h inventory/targets.txt -i "sudo systemctl stop ais_proxy"
parallel-ssh -h inventory/targets.txt -i "sudo systemctl stop ais_target"
parallel-ssh -h inventory/clients.txt -i "sudo systemctl stop ais_proxy"
parallel-ssh -h inventory/clients.txt -i "sudo rm -rf /var/log/ais_proxy/*"
parallel-ssh -h inventory/targets.txt -i "sudo rm -rf /var/log/ais_proxy/*"
parallel-ssh -h inventory/targets.txt -i "sudo rm -rf /var/log/ais_target/*"
parallel-ssh -h inventory/targets.txt -i "sudo rm -rf /etc/ais/bucket-metadata"
parallel-ssh -h inventory/targets.txt -i "sudo rm -rf /etc/ais/smap.json"
parallel-ssh -h inventory/clients.txt -i "sudo rm -rf /etc/ais/smap.json"
parallel-ssh -h inventory/clients.txt -i "sudo rm -rf /etc/ais/bucket-metadata"
