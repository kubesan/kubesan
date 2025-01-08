# SPDX-License-Identifier: Apache-2.0

# This test does not use ksan-supported-modes because it directly tests the
# ThinPoolLv CRD without using Volumes or StorageClass at all.

# delete_thin_lv <name> <index>
# Deletes a thin LV with a given name and its index in the ThinLvs[] array.
delete_thin_lv() {
    name=$1
    index=$2

    ksan-stage "Requesting deletion for thin LV \"$name\"..."
    kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type json --patch "
[
  {"op": "test", "path": "/spec/thinLvs/$index/name", "value": "$name"},
  {"op": "replace", "path": "/spec/thinLvs/$index/state/name", "value": "Removed"}
]
"
    ksan-poll 1 30 "[[ \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs[?(@.name==\"$name\")].state.name}' thinpoollv thinpoollv)\" == \"Removed\" ]]"

    ksan-stage "Removing thin LV \"$name\" from Spec..."
    kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type json --patch "
[
  {"op": "test", "path": "/spec/thinLvs/$index/name", "value": "$name"},
  {"op": "remove", "path": "/spec/thinLvs/$index"}
]
"
    ksan-poll 1 30 "[[ -z \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs[?(@.name==\"$name\")].name}' thinpoollv thinpoollv)\" ]]"
}

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

ksan-stage "Creating snapshot..."
kubectl patch --namespace kubesan-system thinpoollv thinpoollv --type merge --patch "
spec:
  thinLvs:
    - name: thinlv
      contents:
        contentsType: Empty
      readOnly: false
      sizeBytes: 67108864
      state:
        name: Inactive
    - name: snap
      contents:
        contentsType: Snapshot
        snapshot:
          sourceThinLvName: thinlv
      readOnly: false
      sizeBytes: 67108864
      state:
        name: Inactive
"
ksan-poll 1 30 "[[ -n \"\$(kubectl get --namespace kubesan-system -o=jsonpath='{.status.thinLvs[?(@.name==\"snap\")]}' thinpoollv thinpoollv)\" ]]"

delete_thin_lv thinlv 0
delete_thin_lv snap 0

ksan-stage "Deleting ThinPoolLv..."
kubectl delete --namespace kubesan-system thinpoollv thinpoollv
ksan-poll 1 30 "! kubectl get --namespace kubesan-system thinpoollv thinpoollv"
