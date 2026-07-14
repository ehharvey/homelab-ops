#! /bin/bash

DEVCONTAINER_FILE=$(find /workspaces/ -name "devcontainer.json" -type f -print -quit)
USERID=$(id -u)
GROUPID=$(id -g)

echo "User ID: $USERID"
echo "Group ID: $GROUPID"

if [ -z "$DEVCONTAINER_FILE" ]; then
    echo "devcontainer.json not found in /workspaces"
    exit 1
else
    echo "Found devcontainer.json at $DEVCONTAINER_FILE"
fi

cpp -P -E $DEVCONTAINER_FILE | jq '.mounts[]' -r | while read -r mount; do
    echo "Processing mount: $mount"
    # 1 line key=value pairs separated by space
    declare -A mount_array
    while read -d, -r line; do
        # Skip empty pairs
        [[ -z "$line" ]] && echo "Skipping empty line" && continue

        # Parse key=value
        IFS='=' read -r key value <<< "$line"

        mount_array["$key"]="$value"
        echo "Parsed $key=$value"
    done <<< "$mount,"

    target="${mount_array["target"]}"

    if [ -z "$target" ]; then
        echo "No target found for mount: $mount"
        exit 1
    else
        echo "Setting ownership for target: $target with user ID: $USERID and group ID: $GROUPID"
        sudo chown -R $USERID:$GROUPID "$target"
        if [ $? -ne 0 ]; then
            echo "Failed to set ownership for $target"
            exit 1
        else
            echo "Ownership on $target is set to $(ls -ld "$target" | awk '{print $3 ":" $4}')"
        fi
        touched_targets+=("$target")
    fi
done