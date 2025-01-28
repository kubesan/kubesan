# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

ksan-stage 'Provisioning volume...'

ksan-create-rwo-volume test-pvc 32Mi

ksan-stage 'Performing offline resize of unused volume...'
kubectl patch pvc test-pvc --type json --patch '[{"op": "replace", "path": "/spec/resources/requests/storage", "value": "64Mi"}]'
ksan-poll 1 30 '[[ "$(kubectl get pvc test-pvc --no-headers --output custom-columns=CAP:.status.capacity.storage)" == 64Mi ]]'

ksan-stage 'Checking that pod sees new size...'
# Nothing has written to the volume yet, so the entire block should
# initially read as all zeroes.  We then stick a filesystem in that
# area, to prove that the next resize does not mess with our fs, and
# does not leak uninit data in the expanded area.
kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod
    spec:
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cex
            - |
              [[ \$(blockdev --getsize64 /var/pvc) == $((64*1024*1024)) ]] &&
              cmp --bytes $((64*1024*1024)) /dev/zero /var/pvc &&
              mkfs.ext4 /var/pvc
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod"
ksan-wait-for-pod-to-succeed 60 "test-pod"
kubectl delete pod test-pod

ksan-stage 'Performing offline resize of used volume...'
kubectl patch pvc test-pvc --type json --patch '[{"op": "replace", "path": "/spec/resources/requests/storage", "value": "128Mi"}]'
ksan-poll 1 30 '[[ "$(kubectl get pvc test-pvc --no-headers --output custom-columns=CAP:.status.capacity.storage)" == 128Mi ]]'

ksan-stage 'Checking that pod sees new size...'
# The fs we previously wrote should still be 64M, and the remaining 64M
# from the resize must read as all zeroes.
kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod
    spec:
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cex
            - |
              [[ \$(blockdev --getsize64 /var/pvc) == $((128*1024*1024)) ]] &&
              [[ \$(dumpe2fs -h /var/pvc 2>/dev/null | sed -n '/^Block count: */ s///p') == $((64*1024)) ]] &&
              cmp --ignore-initial $((64*1024*1024)) --bytes $((64*1024*1024)) /dev/zero /var/pvc
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod"
ksan-wait-for-pod-to-succeed 60 "test-pod"

ksan-stage 'Unmounting volumes...'
kubectl delete pod test-pod
ksan-delete-volume test-pvc
