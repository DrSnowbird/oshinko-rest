#!/bin/bash

./tools/oshinko-deploy.sh -u $(oc whoami) -p $(oc project --short)
oc status
oc deploy spark-test-m --latest -n spark-cluster
oc deploy spark-test-w --latest -n spark-cluster
