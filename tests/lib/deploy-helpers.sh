#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

cluster_base_name=$( printf 'kubesan-test-%dn' "${num_nodes}" )

__next_cluster() {
    case "$1" in
    kubesan-test-*n-a)
        echo "${cluster_base_name}-b"
        ;;
    kubesan-test-*n-b)
        echo "${cluster_base_name}-a"
        ;;
    *)
        exit 1
        ;;
    esac
}

maintain_two_clusters=0
if (( support_multiple_clusters )); then
    maintain_two_clusters=$(( num_nodes <= 2 ))
fi

__get_a_current_cluster() {
    unset KUBECONFIG

    if (( maintain_two_clusters )); then

        if ! __${deploy_tool}_cluster_exists "${cluster_base_name}-a" &&
            ! __${deploy_tool}_cluster_exists "${cluster_base_name}-b"; then

            current_cluster="${cluster_base_name}-a"

            background_cluster="$( __next_cluster "${current_cluster}" )"
            __create_${deploy_tool}_cluster_async "${background_cluster}"

            __log_cyan "Creating and using ${deploy_tool} cluster '%s'..." "${current_cluster}"
            __start_${deploy_tool}_cluster "${current_cluster}"

        else

            if [[ -n "${current_cluster:-}" && -n "${!:-}" ]]; then
                if kill -0 "$!" &>/dev/null; then
                    __log_cyan "Waiting for ${deploy_tool} cluster '%s' to be ready..." "${background_cluster}"
                fi
                wait || true
                current_cluster="${background_cluster}"
            elif __${deploy_tool}_cluster_exists "${cluster_base_name}-a"; then
                current_cluster="${cluster_base_name}-a"
            else
                current_cluster="${cluster_base_name}-b"
            fi

            background_cluster="$( __next_cluster "${current_cluster}" )"

            if ! __${deploy_tool}_cluster_exists "${background_cluster}"; then
                __create_${deploy_tool}_cluster_async "${background_cluster}"
            fi

            __log_cyan "Using existing ${deploy_tool} cluster '%s'..." "${current_cluster}"
            if ! __is_${deploy_tool}_cluster_running "${current_cluster}"; then
                __restart_${deploy_tool}_cluster "${current_cluster}"
            fi
        fi

    else

        current_cluster="${cluster_base_name}"

        if ! __${deploy_tool}_cluster_exists "${current_cluster}"; then

            __log_cyan "Creating and using ${deploy_tool} cluster '%s'..." "${current_cluster}"
            __start_${deploy_tool}_cluster "${current_cluster}"

        else

            __log_cyan "Using existing ${deploy_tool} cluster '%s'..." "${current_cluster}"
            if ! __is_${deploy_tool}_cluster_running "${current_cluster}"; then
                __restart_${deploy_tool}_cluster "${current_cluster}"
            fi
        fi

    fi

    export current_cluster

    if ! (( create_cache )) && ! (( use_cache )); then
        trap '{
           __delete_${deploy_tool}_cluster "${current_cluster}"
           rm -fr "${temp_dir}"
           __wait_until_background_${deploy_tool}_cluster_is_ready
           }' EXIT
    else
        trap '{
           __stop_${deploy_tool}_cluster "${current_cluster}"
           }' EXIT
    fi
}
export -f __get_a_current_cluster

__clean_background_clusters() {
    trap 'rm -fr "${temp_dir}"' EXIT
    __wait_until_background_${deploy_tool}_cluster_is_ready

    if (( "${creating_cluster_in_background:-0}" == 1 )); then
        minikube stop \
            --profile="${background_cluster}" \
            --keep-context-active \
            --schedule=30m
    fi
}
export -f __clean_background_clusters

__setup_nbd_storage() {

    export NODE_INDICES=( "${!NODES[@]}" )
    export NODE_IPS=()

    for node in "${NODES[@]}"; do
        NODE_IPS+=( "$( __get_${deploy_tool}_node_ip "${current_cluster}" "${node}" )" )
    done

    for node in "${NODES[@]}"; do
        __${deploy_tool}_cp_bashrc "${current_cluster}" "${node}"
    done

    __log_cyan "Starting NBD servers to serve as shared block devices..."

    # Our test cluster only has 6G disks with quite a bit already used
    # by the OS; so we use only 2G per node to form cluster-wide
    # shared storage.  Intentionally dirty the storage, so we can
    # check whether lvm wipes it.
    for (( i = 0; i < 2; ++i )); do
        port=$(( 10809 + i ))
        __${deploy_tool}_ssh "${NODES[i]}" "
            sudo mkdir -p /mnt/vda1
            sudo truncate -s 0 /mnt/vda1/backing${i}.raw
            sudo chmod o+w /mnt/vda1/backing${i}.raw
            printf %$((1024*1024))d 0 > file
            for (( i=0; i<2048; i++ )); do cat file; done >> /mnt/vda1/backing${i}.raw
            rm file
            sudo chmod o-w /mnt/vda1/backing${i}.raw
            __run_in_test_container_async --net host \
                -v /mnt/vda1/backing${i}.raw:/disk${i} -- \
                qemu-nbd --cache=none --format=raw --persistent \
                    --port=${port} --shared=0 /disk${i}
            __run_in_test_container --net host -- bash -c '
                for (( i = 0; i < 50; ++i )); do
                    if nc -z localhost ${port}; then exit 0; fi
                    sleep 0.1
                done
                exit 1
                '
            "
    done

    __log_cyan "Attaching shared block devices to all cluster nodes..."

    for node in "${NODES[@]}"; do
        __${deploy_tool}_ssh "${node}" "
            sudo modprobe nbd nbds_max=16  # for KubeSAN to use as well

            __run_in_test_container --net host -- \
                nbd-client ${NODE_IPS[0]} 10809 /dev/nbd0
            sudo ln -s /dev/nbd0 /dev/kubesan-drive-0
            sudo cp -r /dev/nbd0 /dev/my-san-lun  # good for demos

            __run_in_test_container --net host -- \
                nbd-client ${NODE_IPS[1]} 10810 /dev/nbd1
            sudo ln -s /dev/nbd1 /dev/kubesan-drive-1
            "
    done

    __log_cyan "Configuring LVM on all cluster nodes..."

    for node_index in "${NODE_INDICES[@]}"; do
        __${deploy_tool}_ssh "${NODES[node_index]}" "
            sudo sed -i 's|# use_lvmlockd = 0|use_lvmlockd = 1|' /etc/lvm/lvm.conf
            sudo sed -i 's|# host_id = 0|host_id = $((node_index + 1))|' /etc/lvm/lvmlocal.conf

            # TODO set up watchdog
            sudo sed -i 's|# use_watchdog = 1|use_watchdog = 0|' /etc/sanlock/sanlock.conf

            sudo systemctl restart sanlock lvmlockd
            "
    done

    __log_cyan "Creating shared VG on controller node..."
    __create_ksan_shared_vg kubesan-vg /dev/my-san-lun
}

export -f __setup_nbd_storage
