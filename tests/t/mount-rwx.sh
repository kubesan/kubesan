# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear # TODO add Thin when NBD is implemented

ksan-stage 'Provisioning volumes...'

# Two distinct volumes, to ensure parallel cross-node NBD devices work

ksan-create-rwx-volume test-pvc-1 64Mi
ksan-create-rwx-volume test-pvc-2 128Mi

ksan-stage 'Mounting volumes read-write on all nodes...'

# Two containers per pod: one to keep the pod alive indefinitely
# (useful for debugging the PVC binding), the other that completes
# with success or failure based on the accompanying test script
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
        - name: test
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
        - name: sleep
          image: $TEST_IMAGE
          command:
            - sleep
            - infinity
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
sleep 10
for i in "${!NODES[@]}"; do
    jsonpath='{.status.containerStatuses[?(@.name=="test")].state.terminated.exitCode}'
    [[ "$( kubectl get pod "test-pod-${i}" -o jsonpath="${jsonpath}" )" = 0 ]]
done

ksan-stage 'Unmounting volumes from all nodes...'

kubectl delete pod "${NODE_INDICES[@]/#/test-pod-}" --timeout=30s

ksan-delete-volume test-pvc-1 test-pvc-2
