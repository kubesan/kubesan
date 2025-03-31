#!/bin/bash

set -e

echo "kcli-post-ctl0.sh"

if [ "$(kubectl get nodes -l node-role.kubernetes.io/worker --output=jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | wc -l)" -lt 2 ]; then
    echo "Detected less than 2 workers. Tainting control panel nodes"
    for node in $(kubectl get nodes -l node-role.kubernetes.io/control-plane --output=jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
        echo "Tainting node: $node"
        kubectl taint node $node node-role.kubernetes.io/control-plane-
    done

    echo "Add worker label to all nodes"
    kubectl label nodes --all node-role.kubernetes.io/worker=
fi

base_url=https://github.com/kubernetes-csi/external-snapshotter
ver=v8.2.0

echo "Installing external snapshotter ver ${ver}"
kubectl apply -k "${base_url}/client/config/crd?ref=${ver}"
kubectl apply -k "${base_url}/deploy/kubernetes/snapshot-controller?ref=${ver}"

echo "Creating test vgs"
mkdir -p /etc/lvm/devices
touch /etc/lvm/devices/system.devices

vgn=1
for disk in vdb vdc; do
    dd if=/dev/zero of=/dev/${disk} bs=1M count=16 oflag=direct
    vgcreate --devicesfile test-vg${vgn} --metadatasize 1M --shared test-vg${vgn} /dev/${disk}
    vgn=$(( vgn + 1 ))
done
