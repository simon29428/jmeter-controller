#!/bin/bash


JMETER_ARGS=()

while IFS='=' read -r key value; do
  if [[ $key == *_THREAD_COUNT ]]; then
    JMETER_ARGS+=("-J${key}=${value}")
  fi
done < <(env)

echo "Starting JMeter server with the following arguments:"
echo "${JMETER_ARGS[@]}"

/opt/jmeter/bin/jmeter-server "${JMETER_ARGS[@]}" -Dserver.rmi.ssl.disable=true