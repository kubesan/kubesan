#!/bin/bash
# SPDX-License-Identifier: Apache-2.0

cluster_base_name=kubesan-test

__get_a_current_cluster() {
    unset KUBECONFIG

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

    export current_cluster

    if ! (( create_cache )) && ! (( use_cache )); then
        trap '{
           __delete_${deploy_tool}_cluster "${current_cluster}"
           rm -fr "${temp_dir}"
           }' EXIT
    else
        trap '{
           __stop_${deploy_tool}_cluster "${current_cluster}"
           }' EXIT
    fi
}
export -f __get_a_current_cluster
