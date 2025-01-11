# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

# SOME DEFINITIONS

# Usage: start_pod <one_based_node_index>
start_pod() {
    local pod_name=test-pod-$1
    local node_name=${NODES[$1]}

    kubectl create -f - <<EOF
    apiVersion: v1
    kind: Pod
    metadata:
      name: $pod_name
    spec:
      nodeName: $node_name
      restartPolicy: Never
      containers:
        - name: container
          image: $TEST_IMAGE
          command:
            - fio
            - --name=global
            - --rw=randwrite
            - --fsync=1
            - --direct=1
            - --runtime=60m
            - --time_based=1
            - --filename=/var/pvc
            - --allow_file_create=0
            - --name=job1
          volumeDevices:
            - name: pvc
              devicePath: /var/pvc
          volumeMounts:
            - name: dev
              mountPath: /var/hostDev
          securityContext:
            privileged: true
      volumes:
        - name: pvc
          persistentVolumeClaim:
            claimName: test-pvc
        - name: dev
          hostPath:
            path: /dev
            type: Directory
EOF

    ksan-wait-for-pod-to-start-running 60 "$pod_name"
}

# Usage: ensure_pod_is_writing <one_based_node_index>
ensure_pod_is_writing() {
    local pod_name=test-pod-$1

    sleep 10
    ksan-pod-is-running "$pod_name"
}

# ACTUAL TEST

ksan-stage 'Creating volume...'
ksan-create-rwx-volume test-pvc 64Mi

ksan-stage 'Launching pod mounting the volume and writing to it...'
start_pod 0
ensure_pod_is_writing 0

ksan-stage 'Launching another pod on a different node mounting the volume and writing to it...'
start_pod 1
ensure_pod_is_writing 1

ksan-stage 'Ensuring that the first pod is still writing to the volume...'
ensure_pod_is_writing 0
# CAUTION: this code is fragile - it assumes knowledge of KubeSAN internals.
# This dm device will only exist if the LV is active on the node.
ksan-poll 1 300 "kubectl exec test-pod-0 -- dmsetup status | grep -q 'kubesan--vg-pvc--.*:'"

ksan-stage 'Deleting the first pod...'
kubectl delete pod test-pod-0 --timeout=30s

ksan-stage 'Waiting until the blob pool has migrated...'
ksan-poll 1 300 "kubectl exec test-pod-1 -- dmsetup status | grep -q 'kubesan--vg-pvc--.*:'"

ksan-stage 'Ensuring that the second pod is still writing to the volume...'
ensure_pod_is_writing 1

ksan-stage 'Deleting the second pod...'
kubectl delete pod test-pod-1 --timeout=30s

ksan-delete-volume test-pvc
