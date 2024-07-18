# SPDX-License-Identifier: Apache-2.0

ksan-stage 'Provisioning volumes...'

# Two distinct volumes, to ensure parallel cross-node NBD devices work

for i in 1 2; do
    kubectl create -f - <<EOF
    apiVersion: v1
    kind: PersistentVolumeClaim
    metadata:
      name: test-pvc-$i
    spec:
      accessModes:
        - ReadWriteMany
      resources:
        requests:
          storage: $(( 64 * i ))Mi
      volumeMode: Block
EOF
done

ksan-wait-for-pvc-to-be-bound 300 test-pvc-1
ksan-wait-for-pvc-to-be-bound 300 test-pvc-2

ksan-stage 'Mounting volumes read-write on all nodes...'

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
            - ./mount-rwx-helper.sh
            - "${i}"
            - "${#NODES[@]}"
          volumeDevices:
            - name: pvc-1
              devicePath: /var/pvc1
            - name: pvc-2
              devicePath: /var/pvc2
      volumes:
        - name: pvc-1
          persistentVolumeClaim:
            claimName: test-pvc-1
        - name: pvc-2
          persistentVolumeClaim:
            claimName: test-pvc-2
EOF
done

# Make sure all pods have had a chance to start...
for i in "${!NODES[@]}"; do
    ksan-wait-for-pod-to-start-running 60 "test-pod-$i"
done

# ...at which point, all pods should complete within a few seconds
for i in "${!NODES[@]}"; do
    ksan-wait-for-pod-to-succeed 10 "test-pod-$i"
done

ksan-stage 'Unmounting volumes from all nodes...'

kubectl delete pod "${NODE_INDICES[@]/#/test-pod-}" --timeout=30s

ksan-stage 'Deleting volumes...'

kubectl delete pvc test-pvc-1 test-pvc-2 --timeout=30s
