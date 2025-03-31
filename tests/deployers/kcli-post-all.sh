#!/bin/bash

set -e

mkdir -p /etc/lvm/devices
touch /etc/lvm/devices/system.devices

for vg in test-vg1 test-vg2; do
    vgimportdevices --lock-opt skipgl ${vg} --devicesfile ${vg}
    vgimportdevices --lock-opt skipgl ${vg} --devicesfile dmeventd.devices
    vgchange --devicesfile ${vg} --lockstart ${vg}
done

cat > /etc/systemd/system/kubesan-lockstart.service << EOF
[Unit]
Description=Kubesan lockstart workaround
Requires=sanlock.service lvmlockd.service
After=sanlock.service
After=lvmlockd.service
Before=containerd.service

[Service]
Type=oneshot

ExecStart=/bin/sh -c "vgchange --devicesfile= --lock-start test-vg1 test-vg2"

[Install]
WantedBy=multi-user.target
EOF

restorecon /etc/systemd/system/kubesan-lockstart.service || true
systemctl daemon-reload
systemctl enable --now kubesan-lockstart.service
