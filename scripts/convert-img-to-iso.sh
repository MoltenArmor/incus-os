#!/bin/bash

set -e

# This script converts a raw image with 512 byte sectors to an iso with 2048 byte sectors. The conversion
# allows for booting of the resulting iso as a (virtual) CDROM.

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <input img>"
    exit 1
fi

if [ $EUID -ne 0 ]; then
     echo "This script must be run as root"
     exit 1
fi

SRC=$1
DST="${SRC//.raw/.iso}"

cp "$SRC" "$DST"
truncate --size +1MiB "$DST"
sgdisk -Z "$DST"

SRCLOOPDEV=$(losetup --find --show --partscan "$SRC")
DSTLOOPDEV=$(losetup --sector-size 2048 --find --show "$DST")

PART1GUID=$(sgdisk -i 1 "$SRC" | grep "Partition unique GUID:" | sed -e "s/Partition unique GUID: //")
PART2GUID=$(sgdisk -i 2 "$SRC" | grep "Partition unique GUID:" | sed -e "s/Partition unique GUID: //")
PART3GUID=$(sgdisk -i 3 "$SRC" | grep "Partition unique GUID:" | sed -e "s/Partition unique GUID: //")
PART4GUID=$(sgdisk -i 4 "$SRC" | grep "Partition unique GUID:" | sed -e "s/Partition unique GUID: //")
PART5GUID=$(sgdisk -i 5 "$SRC" | grep "Partition unique GUID:" | sed -e "s/Partition unique GUID: //")
PART3NAME=$(sgdisk -i 3 "$SRC" | grep "Partition name:" | sed -e "s/Partition name: '//" | sed -e "s/'//")
PART4NAME=$(sgdisk -i 4 "$SRC" | grep "Partition name:" | sed -e "s/Partition name: '//" | sed -e "s/'//")
PART5NAME=$(sgdisk -i 5 "$SRC" | grep "Partition name:" | sed -e "s/Partition name: '//" | sed -e "s/'//")

sgdisk -n 1::+2GiB -u "1:$PART1GUID" -t 1:EF00 -c 1:esp "$DSTLOOPDEV"
sgdisk -n 2::+100MiB -u "2:$PART2GUID" -t 2:8300 -c 2:seed-data "$DSTLOOPDEV"
sgdisk -n 3::+16KiB -u "3:$PART3GUID" -t 3:8385 -c "3:$PART3NAME" "$DSTLOOPDEV"
sgdisk -n 4::+100MiB -u "4:$PART4GUID" -t 4:8319 -c "4:$PART4NAME" "$DSTLOOPDEV"
sgdisk -n 5::+1024MiB -u "5:$PART5GUID" -t 5:8314 -c "5:$PART5NAME" "$DSTLOOPDEV"

partprobe "$DSTLOOPDEV"

dd if="${SRCLOOPDEV}p1" of="${DSTLOOPDEV}p1" status=progress
dd if="${SRCLOOPDEV}p2" of="${DSTLOOPDEV}p2" status=progress
dd if="${SRCLOOPDEV}p3" of="${DSTLOOPDEV}p3" status=progress
dd if="${SRCLOOPDEV}p4" of="${DSTLOOPDEV}p4" status=progress
dd if="${SRCLOOPDEV}p5" of="${DSTLOOPDEV}p5" status=progress

losetup -d "$SRCLOOPDEV"
losetup -d "$DSTLOOPDEV"
