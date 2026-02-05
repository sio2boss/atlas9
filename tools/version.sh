#! /bin/sh

# Get path
VER_PATH=$(echo `dirname $0`/install.sh)
cat ${VER_PATH} | grep VERSION | head -1 | awk -F= '{print $2}'