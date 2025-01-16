# SPDX-License-Identifier: Apache-2.0

# usage __create_ksan_sharedvg <vgname> <device>
__create_ksan_shared_vg() {
    __${deploy_tool}_ssh "${NODES[0]}" "
        sudo vgcreate --devicesfile "$1" --shared "$1" "$2"
    "

    # The devices file setup is important:
    # - system.devices must exist so that LVM refrains from scanning all PVs on
    #   the system and only limits itself to a devices file. This ensures that
    #   KubeSAN PVs are not seen by non-KubeSAN LVM users. Although it is not
    #   important in a test environment, it's the setup we recommend in
    #   production and therefore we test it.
    # - dmeventd.devices must contain the KubeSAN PVs so that automatic
    #   extension of thin-pools works.
    # - The <vgname> devices file must contain the KubeSAN PVs. This is the
    #   devices file that KubeSAN's LVM commands use.
    for node in "${NODES[@]}"; do
        __${deploy_tool}_ssh "${node}" "
        sudo touch /etc/lvm/devices/system.devices
        sudo vgimportdevices "$1" --devicesfile "$1"
        sudo vgimportdevices "$1" --devicesfile dmeventd.devices
        sudo vgchange --devicesfile "$1" --lockstart "$1"
        "
    done
}
export -f __create_ksan_shared_vg
