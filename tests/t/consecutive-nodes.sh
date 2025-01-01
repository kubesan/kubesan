# SPDX-License-Identifier: Apache-2.0
#
# This test verifies that the volume can be accessed consecutively from each
# node. This ensures that there are no LV activation leaks after a node
# finishes accessing the volume.

ksan-supported-modes Linear Thin

ksan-stage 'Creating volume...'
ksan-create-rwo-volume test-pvc 64Mi

for i in "${!NODES[@]}"; do
    ksan-stage "Accessing volume from node ${NODES[i]}..."
    kubectl create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: ${NODES[i]}
  containers:
    - name: container
      image: $TEST_IMAGE
      command:
        - bash
        - -c
        - |
          set -o errexit -o pipefail -o nounset -o xtrace
          dd if=/dev/zero of=/var/pvc bs=1M count=64
      volumeDevices:
        - name: test-pvc
          devicePath: /var/pvc
  volumes:
    - name: test-pvc
      persistentVolumeClaim:
        claimName: test-pvc
EOF

    ksan-wait-for-pod-to-succeed 60 test-pod
    kubectl delete pod test-pod --timeout=60s
done

ksan-delete-volume test-pvc
