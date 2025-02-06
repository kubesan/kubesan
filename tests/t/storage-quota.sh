# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

# Demonstrate that a StorageQuoat can limit the size of PVCs.

ksan-stage 'Provisioning 1G volume should succeed...'
ksan-create-rwo-volume test-pvc-1 1Gi

ksan-stage 'Setting up quota to 500Mi...'
kubectl create -f - <<EOF
apiVersion: v1
kind: ResourceQuota
metadata:
  name: storage-quota
spec:
  hard:
    requests.storage: 500Mi
EOF

ksan-stage 'Provisioning 512M volume should now fail to bind...'
ksan-delete-volume test-pvc-1
# Can't use ksan-create-rwo-volume, because we don't want to wait one
# minute for pvc-to-be-bound to fail.
case $(kubectl create -f - 2>&1 <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc-2
spec:
  storageClassName: kubesan
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 512Mi
    limits:
      storage: 1Gi
  volumeMode: Block
EOF
    ) in
    *'exceeded quota'*)
        kubectl get pvc test-pvc-2 && { echo "pvc should not exist"; false; } ;;
    *) echo "expected failure"; false ;;
esac

ksan-stage 'Cleaning up...'
kubectl delete quota storage-quota
