# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

# Prove that data left behind on a previous LV does not prevent use
# for a new LV.  lvcreate should use -Zy -Wn (forcefully wipe first 4k
# to zero without interactively asking if wiping is okay).  This also
# proves that storage starts life as all zeros when first created, but
# then persists even across unstage; and that kubesan does not try to
# mount any filesystem that a pod may have placed in a block device.

ksan-stage 'Provisioning initial volume...'
ksan-create-rwo-volume test-pvc-1 64Mi

ksan-stage 'Checking volume initial contents and populating it...'

kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod-1a
    spec:
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cxe
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
            claimName: test-pvc-1
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod-1a"
ksan-wait-for-pod-to-succeed 60 "test-pod-1a"

ksan-stage 'Checking data persistence...'

kubectl delete pod "test-pod-1a" --timeout=30s
kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod-1b
    spec:
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cxe
            - |
              [[ \$(blockdev --getsize64 /var/pvc) == $((64*1024*1024)) ]] &&
              [[ \$(dumpe2fs -h /var/pvc | sed -n '/^Block count: */ s///p') == $((64*1024)) ]]
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc-1
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod-1b"
ksan-wait-for-pod-to-succeed 60 "test-pod-1b"

ksan-stage 'Removing volume...'

kubectl delete pod "test-pod-1b" --timeout=30s
ksan-delete-volume "test-pvc-1"

ksan-stage 'Provisioning second volume...'
ksan-create-rwo-volume test-pvc-2 64Mi

ksan-stage 'Checking volume initial contents...'

kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: test-pod-2
    spec:
      terminationGracePeriodSeconds: 0
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - bash
            - -cxe
            - |
              [[ \$(blockdev --getsize64 /var/pvc) == $((64*1024*1024)) ]] &&
              cmp --bytes $((64*1024*1024)) /dev/zero /var/pvc
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc-2
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod-2"
ksan-wait-for-pod-to-succeed 60 "test-pod-2"

ksan-stage 'Removing volume...'

kubectl delete pod "test-pod-2" --timeout=30s
ksan-delete-volume "test-pvc-2"
