#!/bin/sh
set -eu

awslocal sqs create-queue --queue-name static-log-analysis-dlq >/dev/null
awslocal sqs create-queue --queue-name static-log-analysis >/dev/null
