# SPDX-License-Identifier: Apache-2.0

ksan-supported-modes Linear Thin

# Linear does not support snapshots, even though the driver advertises it.
# We can still run tests that test argument validation, but must skip all
# tests that require successful snapshots or clones.
skips=''
if [[ $mode == Linear ]]; then
    skips='
        - "--ginkgo.skip=CreateSnapshot.*should succeed"
        - "--ginkgo.skip=CreateSnapshot.*existing name"
        - "--ginkgo.skip=CreateVolume.*existing source"
        - "--ginkgo.skip=DeleteSnapshot.*should return"
        - "--ginkgo.skip=ListSnapshots.*new snapshots"
        - "--ginkgo.skip=ListSnapshots.*next token"
        - "--ginkgo.skip=ListSnapshots.*return snapshots"
'
fi

kubectl create -f - <<EOF
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: csi-parameters
data:
  parameters: '$(kubectl get --output jsonpath={.parameters} sc kubesan)'
---
apiVersion: v1
kind: Pod
metadata:
  name: csi-sanity
spec:
  restartPolicy: Never
  hostPID: true
  # Must run on a node with csi-controller-plugin, but does not care
  # if that pod won or lost any of the leader elections.
  nodeName: $(kubectl get pod --namespace kubesan-system \
        --selector app.kubernetes.io/component==csi-controller-plugin \
        --output custom-columns=NODENAME:.spec.nodeName --no-headers | head -n1)
  containers:
    - name: container
      image: $TEST_IMAGE
      command:
        - ./csi-sanity
        - --csi.controllerendpoint
        - /var/lib/kubelet/plugins/kubesan-controller/socket
        - --csi.endpoint
        - /var/lib/kubelet/plugins/kubesan-node/socket
        - --csi.mountdir
        - /var/lib/kubelet/plugins/csi-sanity-target
        - --csi.stagingdir
        - /var/lib/kubelet/plugins/csi-sanity-staging
        - --csi.testvolumeaccesstype
        - block
        - --csi.testvolumeparameters
        - /etc/csi-parameters/parameters
        # Reduce volume size to fit test shared Volume Group
        - --csi.testvolumesize=$((64*1024*1024))
        - --csi.testvolumeexpandsize=$((128*1024*1024))
        - --ginkgo.v
        - --ginkgo.seed=1
        - --ginkgo.fail-fast
${skips}
      volumeMounts:
        - name: drivers
          mountPath: /var/lib/kubelet/plugins
        - name: csi-parameters
          mountPath: /etc/csi-parameters
        # Mount /dev so that symlinks to block devices resolve
        - name: dev
          mountPath: /dev
      securityContext:
        privileged: true
  volumes:
    - name: drivers
      hostPath:
        path: /var/lib/kubelet/plugins/
        type: DirectoryOrCreate
    - name: csi-parameters
      configMap:
        name: csi-parameters
    - name: dev
      hostPath:
        path: /dev
        type: Directory
EOF

fail=0
ksan-stage "Waiting for csi-sanity results..."
ksan-wait-for-pod-to-succeed $((15*60)) csi-sanity || fail=$?
kubectl logs pods/csi-sanity

kubectl delete configmap csi-parameters
kubectl delete pod csi-sanity

# Test files are sourced as part of a bigger framework where there is
# other cleanup code to be run if this file completes without exiting.
# Directly calling exit would bypass that subsequent code, so we only
# want to exit early if $fail is non-zero.  Because 'set -e' is
# active, that will happen only if this subshell has non-zero status.
(exit $fail)
