# SPDX-License-Identifier: Apache-2.0

# This test does not use ksan-supported-modes because it directly tests the
# ThinPoolLv CRD without using Volumes or StorageClass at all.

ksan-stage "Creating empty ThinPoolLv..."

kubectl create -f - <<EOF
apiVersion: kubesan.gitlab.io/v1alpha1
kind: ThinPoolLv
metadata:
  name: thinpoollv
  namespace: kubesan-system
spec:
  vgName: kubesan-vg
  sizeBytes: 67108864
EOF

# Wait for Status.Conditions["Available"]
ksan-poll 1 30 '[[ "$(ksan-get-condition thinpoollv thinpoollv Available)" == True ]]'

ksan-stage "Creating thin LV..."
kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type merge --patch "
spec:
  activeOnNode: $(__ksan-get-node-name 0)
  thinLvs:
    - name: thinlv
      contents:
        contentsType: Empty
      readOnly: false
      sizeBytes: 67108864
      state:
        name: Inactive
"
ksan-poll 1 30 "[[ -n \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs[?(@.name==\"thinlv\")].name}' thinpoollv thinpoollv)\" ]]"

ksan-stage "Requesting thin LV deletion..."
kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type json --patch '
[
  {"op": "test", "path": "/spec/thinLvs/0/name", "value": "thinlv"},
  {"op": "replace", "path": "/spec/thinLvs/0/state/name", "value": "Removed"}
]
'
ksan-poll 1 30 "[[ \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs[?(@.name==\"thinlv\")].state.name}' thinpoollv thinpoollv)\" = \"Removed\" ]]"

ksan-stage "Removing thin LV from Spec..."
kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type json --patch '
[
  {"op": "test", "path": "/spec/thinLvs/0/name", "value": "thinlv"},
  {"op": "remove", "path": "/spec/thinLvs/0"}
]
'
ksan-poll 1 30 "[[ -z \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs}' thinpoollv thinpoollv)\" ]]"

ksan-stage "Deleting ThinPoolLv..."
kubectl delete --namespace kubesan-system thinpoollv thinpoollv
ksan-poll 1 30 "! kubectl get --namespace kubesan-system thinpoollv thinpoollv"
