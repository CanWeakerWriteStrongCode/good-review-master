#!/bin/bash
go build -o good-review-master .
if [ $? -eq 0 ]; then
    echo "build success: good-review-master"
else
    echo "build failed"
fi
