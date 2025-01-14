#!/usr/bin/env bash

# Copyright 2017 Google LLC All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -x

# k8s.io/code-generator/generate-groups is not module-ready so it breaks things.
export GO111MODULE=off
rm -r $GOPATH/src/agones.dev/agones/pkg/client
rsync -r /go/src/agones.dev/agones/vendor/k8s.io/ /go/src/k8s.io/
cd /go/src/k8s.io/code-generator
./generate-groups.sh "all" \
    agones.dev/agones/pkg/client \
    agones.dev/agones/pkg/apis "allocation:v1 stable:v1alpha1 multicluster:v1alpha1 autoscaling:v1" \
    --go-header-file=/go/src/agones.dev/agones/build/boilerplate.go.txt

