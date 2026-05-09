#!/bin/bash

sleep infinity

jmeter -Dserver.rmi.ssl.disable=true -n -t ${SCRIPT_PATH} -R ${SLAVE_HOSTS} -l ${REPORT_PATH}

echo "Test completed. Stopping the testrun via API call..."

curl -X POST http://jmeter-controller-api.jmeter-system:8080/api/v1/namespaces/${NAMESPACE}/testruns/${TESTRUN_NAME}/stop