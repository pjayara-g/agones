#!/usr/bin/env bash

# Copyright 2019 Google LLC All Rights Reserved.
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

set -ex

header() {
    cat /go/src/agones.dev/agones/build/boilerplate.go.txt ./$1 >> $2/$1
}

googleapis=/go/src/agones.dev/agones/proto/googleapis
protoc_intermediate=/go/src/agones.dev/agones/sdks/cpp/.generated
protoc_destination=/go/src/agones.dev/agones/sdks/cpp

mkdir -p ${protoc_intermediate}
mkdir -p ${protoc_destination}/src/agones
mkdir -p ${protoc_destination}/src/google
mkdir -p ${protoc_destination}/include/agones
mkdir -p ${protoc_destination}/include/google/api

cd /go/src/agones.dev/agones/sdks/cpp
find -name '*.pb.*' -delete
cd /go/src/agones.dev/agones
protoc -I ${googleapis} -I . --grpc_out=${protoc_intermediate} --plugin=protoc-gen-grpc=`which grpc_cpp_plugin` sdk.proto
protoc -I ${googleapis} -I . --cpp_out=dllexport_decl=AGONES_EXPORT:${protoc_intermediate} sdk.proto ${googleapis}/google/api/annotations.proto ${googleapis}/google/api/http.proto

cd ${protoc_intermediate}
header sdk.grpc.pb.cc ${protoc_destination}/src/agones
header sdk.pb.cc ${protoc_destination}/src/agones
header sdk.grpc.pb.h ${protoc_destination}/include/agones
header sdk.pb.h ${protoc_destination}/include/agones

cd ${protoc_intermediate}/google/api
header annotations.pb.cc ${protoc_destination}/src/google
header http.pb.cc ${protoc_destination}/src/google
header annotations.pb.h ${protoc_destination}/include/google/api
header http.pb.h ${protoc_destination}/include/google/api

rm -r ${protoc_intermediate}
