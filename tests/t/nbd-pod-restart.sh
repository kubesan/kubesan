# SPDX-License-Identifier: Apache-2.0

# This test checks that a node using secondary access (the NBD client)
# can survive a reset of the node manager pod, since an NBD client is
# independent of the manager once set up.  (Note, however, that the
# converse of the primary node manager being reset while serving NBD is
# a non-goal, as resetting kills the NBD connection).
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

ksan-stage 'Forcing restart of node manager...'
# Kill the node manager...
find_manager() {
    kubectl --namespace kubesan-system get pods --output name \
            --field-selector spec.nodeName=${NODES[1]} \
            --selector app.kubernetes.io/component=node-controller-manager
}
victim=$(find_manager)
kubectl delete --namespace kubesan-system ${victim} --timeout=30s

# ...then wait for the DaemonSet to respawn its replacement
ksan-poll 1 30 "[[ \$(find_manager | wc -l) == 1 && \$(find_manager) != ${victim} ]]"

ksan-stage 'Ensuring pods survived...'
sleep 2
for i in "${!NODES[@]}"; do
    ksan-pod-is-running "test-pod-$i"
done

ksan-stage 'Unmounting volume from all nodes...'

kubectl delete pod "${NODE_INDICES[@]/#/test-pod-}" --timeout=30s

ksan-delete-volume test-pvc
