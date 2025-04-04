# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

ksan-stage 'Creating second StorageClass'

kubectl create -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: second
  annotations:
    storageclass.kubernetes.io/is-default-class: "false"
provisioner: kubesan.gitlab.io
parameters:
  lvmVolumeGroup: test-vg2
  mode: ${mode^}
EOF

ksan-stage 'Provisioning volumes in each StorageClass...'

# make_pvc sc_name
make_pvc()
{
    local sc_name="$1"

    kubectl create -f - <<EOF
    apiVersion: v1
    kind: PersistentVolumeClaim
    metadata:
      name: test-pvc-${sc_name}
    spec:
      accessModes:
        - ReadWriteOnce
      resources:
        requests:
          storage: 64Mi
      volumeMode: Block
      storageClassName: ${sc_name}
EOF
}

make_pvc kubesan-$mode
make_pvc second

ksan-wait-for-pvc-to-be-bound 60 "test-pvc-kubesan-$mode"
ksan-wait-for-pvc-to-be-bound 60 "test-pvc-second"

ksan-stage 'Mounting both volumes read-write...'

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
        - -c
        - |
          dd if=/var/pvc1 of=/var/pvc2 bs=1M count=64 oflag=direct &&
          sleep infinity
      volumeDevices:
        - name: pvc1
          devicePath: /var/pvc1
        - name: pvc2
          devicePath: /var/pvc2
  volumes:
    - name: pvc1
      persistentVolumeClaim:
        claimName: test-pvc-kubesan-$mode
    - name: pvc2
      persistentVolumeClaim:
        claimName: test-pvc-second
EOF

ksan-wait-for-pod-to-start-running 60 "test-pod"
ksan-pod-is-running "test-pod"

ksan-stage 'Unmounting volumes...'

kubectl delete pod "test-pod" --timeout=30s

ksan-delete-volume "test-pvc-kubesan-$mode" "test-pvc-second"
