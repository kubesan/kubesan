# SPDX-License-Identifier: Apache-2.0
#
# This test verifies that the thin pool grows automatically when a volume is
# overwritten while snapshots still reference old blocks. Worst case data
# consumption occurs when a snapshot is created and the volume is subsequently
# completely overwritten.

ksan-supported-modes Thin

ksan-stage "Creating volume..."
ksan-create-rwo-volume test-pvc-1 512Mi

ksan-fill-volume test-pvc-1 512

ksan-stage "Creating snapshot 1..."
ksan-create-snapshot test-pvc-1 test-vs-1

ksan-fill-volume test-pvc-1 512

ksan-stage "Creating snapshot 2..."
ksan-create-snapshot test-pvc-1 test-vs-2

ksan-fill-volume test-pvc-1 512

ksan-delete-volume test-pvc-1
ksan-delete-snapshot test-vs-1
ksan-delete-snapshot test-vs-2
