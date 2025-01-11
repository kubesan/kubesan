# SPDX-License-Identifier: Apache-2.0

# This test checks that RWX connections survive pod restarts.  This
# works for both the secondary node (the NBD client connection is
# independent of the pod once started; and the new pod sees the
# existing client), and the primary node (the NBD server has a
# graceful shutdown that waits for all clients to first disconnect
# before the new pod starts a new server that clients can connect to).
ksan-supported-modes Linear Thin

ksan-stage 'Provisioning volume...'
ksan-create-rwx-volume test-pvc 64Mi

ksan-stage 'Mounting volume read-write on all nodes...'

start_pod() {
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
            - sh
            - -c
            - "while :; do dd if=/dev/urandom of=/var/pvc count=1 bs=64k status=none || exit; sleep 1; done"
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc
EOF
}

# Stagger the starts, to ensure only node 0 is running an NBD server
for i in "${!NODES[@]}"; do
    start_pod $i
    ksan-wait-for-pod-to-start-running 60 "test-pod-$i"
done

# find_manager <idx>: return the name of the node manager pod on node idx
find_manager() {
    kubectl --namespace kubesan-system get pods --output name \
            --field-selector spec.nodeName=${NODES[$1]} \
            --selector app.kubernetes.io/component=node-controller-manager
}

ksan-stage 'Forcing restart of secondary node manager...'
# Kill the node manager...
victim=$(find_manager 1)
kubectl delete --namespace kubesan-system ${victim} --timeout=30s

# ...then wait for the DaemonSet to respawn its replacement
ksan-poll 1 30 "[[ \$(find_manager 1 | wc -l) == 1 && \$(find_manager 1) != ${victim} ]]"

ksan-stage 'Ensuring pods survived...'
sleep 2
for i in "${!NODES[@]}"; do
    ksan-pod-is-running "test-pod-$i"
done

ksan-stage 'Forcing restart of primary node manager...'
# Kill the node manager...
victim=$(find_manager 0)
kubectl delete --namespace kubesan-system ${victim} --timeout=30s

# ...then wait for the DaemonSet to respawn its replacement
ksan-poll 1 30 "[[ \$(find_manager 0 | wc -l) == 1 && \$(find_manager 0) != ${victim} ]]"

ksan-stage 'Ensuring pods survived...'
sleep 2
for i in "${!NODES[@]}"; do
    ksan-pod-is-running "test-pod-$i"
done

ksan-stage 'Unmounting volume from all nodes...'

kubectl delete pod "${NODE_INDICES[@]/#/test-pod-}" --timeout=30s

ksan-delete-volume test-pvc
