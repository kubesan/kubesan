# SPDX-License-Identifier: Apache-2.0

____run_in_test_container_aux() {
    local __opts=()

    while [[ "$1" == -* ]]; do
        while (( $# > 0 )); do
            if [[ "$1" == -- ]]; then
                shift
                break
            else
                __opts+=( "$1" )
                shift
            fi
        done
    done

    sudo podman --url=unix:///run/podman/podman.sock run --privileged -t -v /dev:/dev "${__opts[@]}" \
        192.168.122.253:5000/kubesan/test:test "$@"
}

__run_in_test_container() {
    ____run_in_test_container_aux -i --rm -- "$@"
}

__run_in_test_container_async() {
    ____run_in_test_container_aux -d -- "$@"
}
