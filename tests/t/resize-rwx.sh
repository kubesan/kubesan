# SPDX-License-Identifier: Apache-2.0

# Linear cannot resize an online image unless we add a dm-linear wrapper.
ksan-supported-modes Thin

ksan-stage 'Provisioning volume...'

ksan-create-rwx-volume test-pvc 64Mi

ksan-stage 'Using pvc from multiple nodes...'
# Nothing in these pods write, so the entire block should read as all zeroes.
# This tests that expansion works without exposing uninitialized data.
for i in "${!NODES[@]}"; do
    kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod-$i
    spec:
      nodeName: ${NODES[i]}
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cex
            - |
              while :; do
                  if [[ \$(blockdev --getsize64 /var/pvc) == $((128*1024*1024)) ]]; then
                      break
                  fi
                  sleep 1
              done
              cmp --bytes $((128*1024*1024)) /dev/zero /var/pvc
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc
EOF
done

for i in "${!NODES[@]}"; do
    ksan-wait-for-pod-to-start-running 60 "test-pod-$i"
done
for i in "${!NODES[@]}"; do
    ksan-pod-is-running "test-pod-$i"
done

ksan-stage 'Performing online resize...'
kubectl patch pvc test-pvc --type json --patch '[{"op": "replace", "path": "/spec/resources/requests/storage", "value": "128Mi"}]'
ksan-poll 1 30 '[[ "$(kubectl get pvc test-pvc --no-headers --output custom-columns=CAP:.status.capacity.storage)" == 128Mi ]]'

for i in "${!NODES[@]}"; do
    ksan-wait-for-pod-to-succeed 60 "test-pod-$i"
done

ksan-stage 'Unmounting volumes...'
kubectl delete pod "${NODE_INDICES[@]/#/test-pod-}" --timeout=30s
ksan-delete-volume test-pvc
